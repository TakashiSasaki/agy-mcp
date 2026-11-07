package manager

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

const (
	DefaultEvidenceBytes   = 64 << 10
	MaxEvidenceBytes       = 256 << 10
	maxJournalRecord       = 9 << 20
	maxInlineEvent         = 16 << 10
	streamEvents           = "events"
	streamArtifact         = "artifact"
	agentOutcomeError      = "error"
	evidenceOutcomeUnknown = "unknown"
)

type LogRequest struct {
	JobID, Stream, Cursor string
	Path                  string
	MaxBytes, TailBytes   int
	FromStart             bool
}

type LogPage struct {
	JobID       string            `json:"job_id" jsonschema:"job owning this stream"`
	State       string            `json:"state" jsonschema:"current job state, not a command or test verdict"`
	Stream      string            `json:"stream" jsonschema:"stdout, stderr, events or artifact"`
	Source      string            `json:"source" jsonschema:"assistant_text, agy_stderr, timestamped_cli_events or artifact_file; stdout is not individual shell-command stdout"`
	Path        string            `json:"path,omitempty" jsonschema:"requested job-associated path for an artifact stream"`
	Available   bool              `json:"available" jsonschema:"whether this stream exists for the job"`
	StartOffset int64             `json:"start_offset" jsonschema:"starting byte offset in the original file"`
	EndOffset   int64             `json:"end_offset" jsonschema:"exclusive ending byte offset in the original file"`
	SizeBytes   int64             `json:"size_bytes" jsonschema:"stream size observed before reading"`
	Content     string            `json:"content" jsonschema:"UTF-8 text from this byte range; invalid UTF-8 is replaced"`
	SHA256      string            `json:"sha256,omitempty" jsonschema:"SHA-256 of this page's original bytes, not the complete growing stream"`
	HasMore     bool              `json:"has_more" jsonschema:"more complete bytes can be read from this observed snapshot; false when only pending_utf8 bytes remain"`
	PendingUTF8 bool              `json:"pending_utf8,omitempty" jsonschema:"a running job's incomplete UTF-8 suffix is withheld at the cursor; wait for append or job completion before retrying next_cursor"`
	NextCursor  string            `json:"next_cursor,omitempty" jsonschema:"resume at this byte position; retain it even at EOF to follow later appends"`
	Capture     *jobstore.Capture `json:"capture,omitempty" jsonschema:"event capture health when known; absence is unknown, including older jobs"`
}

type EventsRequest struct {
	JobID, Cursor   string
	Limit, MaxBytes int
}

type EventEntry struct {
	Offset         int64          `json:"offset" jsonschema:"stable byte offset of this journal record"`
	ObservedAt     string         `json:"observed_at" jsonschema:"UTC time the supervisor observed this CLI event"`
	Kind           string         `json:"kind" jsonschema:"provider event kind, such as init, step_update or result"`
	StepIndex      *int           `json:"step_index,omitempty" jsonschema:"CLI step index, including zero, when supplied"`
	StepType       string         `json:"step_type,omitempty" jsonschema:"CLI step type; tool alone does not prove a shell command ran"`
	StepState      string         `json:"step_state,omitempty" jsonschema:"CLI step lifecycle state; distinct from process exit status"`
	Payload        map[string]any `json:"payload,omitempty" jsonschema:"original CLI event fields; provider-reported data, not inferred from model narrative"`
	PayloadOmitted bool           `json:"payload_omitted" jsonschema:"true when the raw payload exceeded the inline/page limit; read events with agy_logs for the original record"`
	RecordBytes    int            `json:"record_bytes" jsonschema:"byte length of the original journal record including its newline"`
}

type EventsPage struct {
	JobID                string                   `json:"job_id" jsonschema:"job owning these events"`
	State                string                   `json:"state" jsonschema:"job state; done does not establish shell-command or test success"`
	Available            bool                     `json:"available" jsonschema:"false for jobs with no captured journal, including jobs started by an older supervisor"`
	Events               []EventEntry             `json:"events" jsonschema:"captured CLI events in arrival order"`
	NextCursor           string                   `json:"next_cursor,omitempty" jsonschema:"resume this journal generation; retain at EOF to follow appended events"`
	HasMore              bool                     `json:"has_more" jsonschema:"more complete records may be read from this observed snapshot"`
	PendingRecord        bool                     `json:"pending_record" jsonschema:"a trailing incomplete record was withheld; retry the same next_cursor after progress"`
	IncompleteRecord     bool                     `json:"incomplete_record" jsonschema:"a trailing incomplete record was withheld; after capture ends it is skipped as lost evidence"`
	SkippedRecords       int                      `json:"skipped_records" jsonschema:"malformed or oversized records skipped in this page; this is evidence loss"`
	Capture              *jobstore.Capture        `json:"capture,omitempty" jsonschema:"capture generation, limit and completeness; closed is not task success"`
	Process              *jobstore.ProcessOutcome `json:"process,omitempty" jsonschema:"supervisor-observed outcome of agy itself, distinct from nested shell commands"`
	AgentOutcome         string                   `json:"agent_outcome" jsonschema:"success or error only from the CLI terminal result; unknown otherwise"`
	VerificationOutcome  string                   `json:"verification_outcome" jsonschema:"not_checked; inspect explicit verification reports with agy_verification"`
	ToolExecutionDetails string                   `json:"tool_execution_details" jsonschema:"raw_cli_only; command fields and exit codes are available only if the provider emits them, and are never invented"`
}

type evidenceCursor struct {
	Version    int    `json:"v"`
	JobID      string `json:"job"`
	Stream     string `json:"stream"`
	Generation string `json:"generation"`
	Offset     int64  `json:"offset"`
	Anchor     string `json:"anchor"`
}

func evidenceLimit(value int) (int, error) {
	if value == 0 {
		return DefaultEvidenceBytes, nil
	}
	if value < 4 || value > MaxEvidenceBytes {
		return 0, fmt.Errorf("max_bytes must be between 4 and %d", MaxEvidenceBytes)
	}
	return value, nil
}

func evidenceAnchor(f *os.File, offset int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, min(offset, 128))); err != nil {
		return "", err
	}
	if _, err := io.Copy(h, io.NewSectionReader(f, max(0, offset-128), min(offset, 128))); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func evidencePosition(f *os.File, meta jobstore.Meta, stream string, capture *jobstore.Capture, cursor string) (evidenceCursor, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return evidenceCursor{}, 0, err
	}
	if !info.Mode().IsRegular() {
		return evidenceCursor{}, 0, fmt.Errorf("evidence stream is not a regular file")
	}
	identity, err := fileIdentity(f)
	if err != nil {
		return evidenceCursor{}, 0, err
	}
	generation := meta.ID + ":" + meta.StartedAt.UTC().Format(time.RFC3339Nano) + ":" + identity
	if stream == streamEvents && capture != nil {
		generation += ":" + capture.Generation
	}
	digest := sha256.Sum256([]byte(generation))
	want := evidenceCursor{Version: 1, JobID: meta.ID, Stream: stream, Generation: hex.EncodeToString(digest[:])}
	if cursor == "" {
		return want, info.Size(), nil
	}
	if len(cursor) > 4096 {
		return want, 0, fmt.Errorf("invalid evidence cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	var got evidenceCursor
	if err != nil || json.Unmarshal(data, &got) != nil {
		return want, 0, fmt.Errorf("invalid evidence cursor")
	}
	if got.Offset < 0 || got.Offset > info.Size() {
		return want, 0, fmt.Errorf("evidence stream was truncated or cursor offset is invalid")
	}
	anchor, err := evidenceAnchor(f, got.Offset)
	if err != nil {
		return want, 0, err
	}
	position := got.Offset
	got.Offset = 0
	if got.Anchor != anchor {
		return want, 0, fmt.Errorf("evidence before this cursor changed")
	}
	got.Anchor = ""
	if got != want {
		return want, 0, fmt.Errorf("cursor belongs to another job, stream or generation")
	}
	want.Offset = position
	return want, info.Size(), nil
}

func encodeEvidenceCursor(f *os.File, c evidenceCursor) (string, error) {
	anchor, err := evidenceAnchor(f, c.Offset)
	if err != nil {
		return "", err
	}
	c.Anchor = anchor
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (m *Manager) openEvidence(id, stream string) (jobstore.Meta, string, *os.File, *jobstore.Capture, error) {
	meta, err := m.store.Load(id)
	if err != nil {
		return meta, "", nil, nil, err
	}
	dir, err := m.store.Dir(id)
	if err != nil {
		return meta, "", nil, nil, err
	}
	capture, err := jobstore.ReadCaptureDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return meta, dir, nil, nil, err
	}
	var name string
	switch stream {
	case "stdout":
		name = jobstore.OutFile
	case "stderr":
		name = jobstore.ErrFile
	case streamEvents:
		name = jobstore.EventsFile
	default:
		return meta, dir, nil, capture, fmt.Errorf("stream must be stdout, stderr or events")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return meta, dir, nil, capture, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, artifactReadFlags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return meta, dir, nil, capture, nil
	}
	return meta, dir, f, capture, err
}

func (m *Manager) openLogEvidence(req LogRequest) (jobstore.Meta, string, *os.File, *jobstore.Capture, error) {
	if req.Stream != streamArtifact {
		if req.Path != "" {
			return jobstore.Meta{}, "", nil, nil, fmt.Errorf("path is only valid for the artifact stream")
		}
		return m.openEvidence(req.JobID, req.Stream)
	}
	meta, dir, roots, err := m.resolveJobAndRoots(req.JobID)
	if err != nil {
		return meta, dir, nil, nil, err
	}
	target, err := artifactTarget(meta, dir, req.Path)
	if err != nil {
		return meta, dir, nil, nil, err
	}
	f, err := openArtifactFile(roots, target)
	return meta, dir, f, nil, err
}

func (m *Manager) Logs(req LogRequest) (LogPage, error) {
	limit, err := evidenceLimit(req.MaxBytes)
	if err != nil {
		return LogPage{}, err
	}
	if req.TailBytes < 0 || req.TailBytes > 4<<20 || (req.FromStart && (req.Cursor != "" || req.TailBytes != 0)) || (req.Cursor != "" && req.TailBytes != 0) {
		return LogPage{}, fmt.Errorf("invalid or conflicting log start options")
	}
	meta, _, f, capture, err := m.openLogEvidence(req)
	if err != nil {
		return LogPage{}, err
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	st, err := m.Status(req.JobID)
	if err != nil {
		return LogPage{}, err
	}
	out := LogPage{JobID: req.JobID, State: st.State, Stream: req.Stream, Capture: capture}
	switch req.Stream {
	case "stdout":
		out.Source = "assistant_text"
	case "stderr":
		out.Source = "agy_stderr"
	case streamEvents:
		out.Source = "timestamped_cli_events"
	case streamArtifact:
		out.Source = "artifact_file"
		out.Path = req.Path
	}
	if f == nil {
		if req.Cursor != "" {
			return out, fmt.Errorf("evidence stream for this cursor is missing")
		}
		return out, nil
	}
	streamKey := req.Stream
	if req.Stream == streamArtifact {
		streamKey += ":" + req.Path
	}
	c, size, err := evidencePosition(f, meta, streamKey, capture, req.Cursor)
	if err != nil {
		return out, err
	}
	if req.Cursor == "" && !req.FromStart {
		tail := req.TailBytes
		if tail == 0 {
			tail = limit
		}
		c.Offset = max(0, size-int64(tail))
	}
	out.Available = true
	out.SizeBytes = size
	out.StartOffset = c.Offset
	data, readStart, pending, err := readLogBytes(f, c.Offset, size, limit, req.Cursor == "" && c.Offset > 0, st.State == StateRunning)
	if err != nil {
		return out, err
	}
	c.Offset = readStart
	out.StartOffset = readStart
	out.Content = strings.ToValidUTF8(string(data), "\uFFFD")
	digest := sha256.Sum256(data)
	out.SHA256 = hex.EncodeToString(digest[:])
	c.Offset += int64(len(data))
	out.EndOffset = c.Offset
	out.HasMore = c.Offset < size && !pending
	out.PendingUTF8 = pending
	out.NextCursor, err = encodeEvidenceCursor(f, c)
	return out, err
}

func decodeJournalEntry(line []byte, offset int64, remaining int) (EventEntry, error) {
	var record jobstore.JournalRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return EventEntry{}, err
	}
	var event streamjson.Event
	if err := json.Unmarshal(record.Event, &event); err != nil || record.ObservedAt.IsZero() || event.Kind == "" {
		return EventEntry{}, fmt.Errorf("malformed journal record")
	}
	out := EventEntry{Offset: offset, ObservedAt: record.ObservedAt.UTC().Format(time.RFC3339Nano), Kind: event.Kind, RecordBytes: len(line), PayloadOmitted: true}
	if step := event.StepUpdate; step != nil {
		out.StepIndex = new(step.StepIndex)
		out.StepType = step.StepType
		out.StepState = step.State
	}
	if len(record.Event) <= maxInlineEvent && len(record.Event)+512 <= remaining {
		decoder := json.NewDecoder(bytes.NewReader(record.Event))
		decoder.UseNumber()
		if err := decoder.Decode(&out.Payload); err != nil {
			return out, err
		}
		out.PayloadOmitted = false
	}
	return out, nil
}

func (m *Manager) Events(req EventsRequest) (EventsPage, error) {
	budget, err := evidenceLimit(req.MaxBytes)
	if err != nil {
		return EventsPage{}, err
	}
	if budget < 1024 {
		return EventsPage{}, fmt.Errorf("event max_bytes must be at least 1024")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return EventsPage{}, fmt.Errorf("limit must be between 1 and 200")
	}
	meta, dir, f, capture, err := m.openEvidence(req.JobID, streamEvents)
	if err != nil {
		return EventsPage{}, err
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	st, err := m.Status(req.JobID)
	if err != nil {
		return EventsPage{}, err
	}
	out := EventsPage{JobID: req.JobID, State: st.State, Events: []EventEntry{}, Capture: capture, AgentOutcome: evidenceOutcomeUnknown, VerificationOutcome: "not_checked", ToolExecutionDetails: "raw_cli_only"}
	out.Process, out.AgentOutcome, err = readEvidenceOutcomes(dir)
	if err != nil {
		return out, err
	}
	if f == nil {
		if req.Cursor != "" {
			return out, fmt.Errorf("event journal for this cursor is missing")
		}
		return out, nil
	}
	c, size, err := evidencePosition(f, meta, streamEvents, capture, req.Cursor)
	if err != nil {
		return out, err
	}
	out.Available = true
	reader := bufio.NewReader(io.NewSectionReader(f, c.Offset, size-c.Offset))
	if err := scanEventRecords(reader, &c, size, budget, limit, &out); err != nil {
		return out, err
	}
	out.HasMore = c.Offset < size && !out.PendingRecord
	out.NextCursor, err = encodeEvidenceCursor(f, c)
	return out, err
}

func readJournalLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > maxJournalRecord {
			return nil, fmt.Errorf("journal record exceeds %d bytes", maxJournalRecord)
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func readEvidenceOutcomes(dir string) (*jobstore.ProcessOutcome, string, error) {
	process, err := jobstore.ReadProcessDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, evidenceOutcomeUnknown, err
	}
	outcome := evidenceOutcomeUnknown
	if data, err := jobstore.ReadResultDir(dir); err == nil && len(data) > 0 {
		var result streamjson.Result
		if json.Unmarshal(data, &result) == nil {
			switch result.Status {
			case streamjson.StatusSuccess:
				outcome = "success"
			case streamjson.StatusError:
				outcome = agentOutcomeError
			}
		}
	}
	return process, outcome, nil
}

func scanEventRecords(reader *bufio.Reader, c *evidenceCursor, size int64, budget, limit int, out *EventsPage) error {
	readBytes := 0
	used := 0
	for scanned := 0; scanned < limit && c.Offset < size && readBytes < maxJournalRecord; scanned++ {
		line, readErr := readJournalLine(reader)
		readBytes += len(line)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' {
			out.IncompleteRecord = true
			out.PendingRecord = out.State == StateRunning && (out.Capture == nil || !out.Capture.Closed)
			if !out.PendingRecord {
				out.SkippedRecords++
				c.Offset = size
			}
			break
		}
		entry, decodeErr := decodeJournalEntry(line, c.Offset, budget-used)
		if decodeErr != nil {
			out.SkippedRecords++
			c.Offset += int64(len(line))
			continue
		}
		encoded, _ := json.Marshal(entry)
		if used+len(encoded) > budget && len(out.Events) > 0 {
			break
		}
		if used+len(encoded) > budget {
			return fmt.Errorf("max_bytes too small for event metadata")
		}
		out.Events = append(out.Events, entry)
		used += len(encoded)
		c.Offset += int64(len(line))
	}
	return nil
}

func readLogBytes(f *os.File, offset, size int64, limit int, trimStart, live bool) (content []byte, nextStart int64, pending bool, readErr error) {
	if trimStart {
		// Look behind the tail boundary to distinguish a split valid rune from
		// standalone continuation bytes. Only the former should be skipped.
		before := max(0, offset-utf8.UTFMax+1)
		around, err := io.ReadAll(io.NewSectionReader(f, before, min(int64(2*utf8.UTFMax-1), size-before)))
		if err != nil {
			return nil, offset, false, err
		}
		for i := 0; i < len(around) && int64(i) < offset-before; i++ {
			if live && !utf8.FullRune(around[i:]) {
				// A tail may begin inside the incomplete suffix. Keep its
				// leading byte so an append can still complete the character.
				offset = before + int64(i)
				break
			}
			_, width := utf8.DecodeRune(around[i:])
			if width > 1 && before+int64(i+width) > offset {
				offset = before + int64(i+width)
				break
			}
		}
	}
	data, err := io.ReadAll(io.NewSectionReader(f, offset, min(int64(limit+utf8.UTFMax), size-offset)))
	if err != nil {
		return nil, offset, false, err
	}
	end := 0
	for end < len(data) && end < limit {
		if live && !utf8.FullRune(data[end:]) {
			pending = true
			break
		}
		_, width := utf8.DecodeRune(data[end:])
		if end+width > limit {
			break
		}
		// DecodeRune consumes one byte for invalid encoding, guaranteeing
		// progress without splitting a valid rune across pages.
		end += width
	}
	return data[:end], offset, pending, nil
}
