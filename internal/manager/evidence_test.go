package manager

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func stageEvidenceJob(t *testing.T, live bool) (mgr *Manager, stateStore *jobstore.Store, jobID, jobDir string) {
	t.Helper()
	m := newManager(t, managerOpts{jobTTL: time.Hour})
	store := jobstore.New(m.cfg.StateDir)
	meta := jobstore.Meta{ID: "evidence-job", Cwd: t.TempDir(), StartedAt: time.Now().UTC(), Args: []string{"--output-format", "stream-json"}}
	if live {
		ticks, ok := m.readStartTimeTicks(os.Getpid())
		if !ok || ticks == 0 {
			t.Skip("live process identity unavailable")
		}
		meta.PID = os.Getpid()
		meta.StartTimeTicks = ticks
		meta.BootID = readBootID()
	}
	dir, err := store.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		if err := store.WriteExitCode(meta.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	return m, store, meta.ID, dir
}

// Mutation guards: file identity/cursor matching and UTF-8 boundary in Logs.
func TestEvidenceLogsFollowAndBind(t *testing.T) {
	m, store, id, dir := stageEvidenceJob(t, false)
	path := jobstore.OutPath(dir)
	if err := os.WriteFile(path, []byte("あ😀éZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	cursor := ""
	for range 8 {
		page, err := m.Logs(LogRequest{JobID: id, Stream: "stdout", FromStart: cursor == "", Cursor: cursor, MaxBytes: 4})
		if err != nil {
			t.Fatal(err)
		}
		output.WriteString(page.Content)
		cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if output.String() != "あ😀éZ" {
		t.Fatalf("UTF-8 pages changed text: %q", output.String())
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" appended"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	page, err := m.Logs(LogRequest{JobID: id, Stream: "stdout", Cursor: cursor})
	if err != nil || page.Content != " appended" || page.Source != "assistant_text" {
		t.Fatalf("append follow: %+v %v", page, err)
	}
	if m.Collected(id) {
		t.Fatal("log read collected the job")
	}
	changed, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.WriteAt([]byte("XYZ"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Logs(LogRequest{JobID: id, Stream: "stdout", Cursor: cursor}); err == nil {
		t.Fatal("accepted a cursor after preceding bytes changed")
	}
	if _, err := changed.WriteAt([]byte("あ"), 0); err != nil {
		t.Fatal(err)
	}
	_ = changed.Close()
	if err := os.WriteFile(jobstore.ErrPath(dir), []byte("stderr"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", Cursor: cursor}); err == nil {
		t.Fatal("accepted cursor for a different stream")
	}
	meta, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	meta.ID = "another"
	other, err := store.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.OutPath(other), []byte("あ😀éZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Logs(LogRequest{JobID: "another", Stream: "stdout", Cursor: cursor}); err == nil {
		t.Fatal("accepted cursor for another job")
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("あ😀éZ appended"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Logs(LogRequest{JobID: id, Stream: "stdout", Cursor: cursor}); err == nil {
		t.Fatal("accepted replaced stream with identical bytes")
	}
}

// Mutation guards: no incomplete event emission, per-page limits, original
// payload retention, and journal generation binding in Events.
func TestEvidenceEventsLiveAndTerminal(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, true)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.Append([]byte(`{"event":"step_update","step_update":{"step_index":0,"step_type":"tool","state":"ACTIVE","command":"pytest -q","exit_code":null,"execution_id":9007199254740993}}`))
	page, err := m.Events(EventsRequest{JobID: id, Limit: 1})
	if err != nil || page.State != StateRunning || len(page.Events) != 1 || page.Events[0].StepIndex == nil || *page.Events[0].StepIndex != 0 {
		t.Fatalf("live event: %+v %v", page, err)
	}
	payload := page.Events[0].Payload["step_update"].(map[string]any)
	if payload["command"] != "pytest -q" || payload["exit_code"] != nil || page.VerificationOutcome != "not_checked" {
		t.Fatalf("provider data was invented/lost: %+v", page)
	}
	if number, ok := payload["execution_id"].(json.Number); !ok || number.String() != "9007199254740993" {
		t.Fatal("provider execution identity was rounded", payload["execution_id"])
	}
	cursor := page.NextCursor
	partial := `{"observed_at":"2026-11-03T00:00:00Z","event":{"event":"init"`
	f, err := os.OpenFile(filepath.Join(dir, jobstore.EventsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(partial); err != nil {
		t.Fatal(err)
	}
	page, err = m.Events(EventsRequest{JobID: id, Cursor: cursor})
	if err != nil || len(page.Events) != 0 || !page.PendingRecord || !page.IncompleteRecord || page.NextCursor != cursor {
		t.Fatalf("incomplete record escaped or cursor advanced: %+v %v", page, err)
	}
	if _, err := f.WriteString("}}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	page, err = m.Events(EventsRequest{JobID: id, Cursor: cursor})
	if err != nil || len(page.Events) != 1 || page.PendingRecord {
		t.Fatalf("completed append was lost: %+v %v", page, err)
	}
	journal.Close()
	if m.Collected(id) {
		t.Fatal("event reads collected the result")
	}
}

// Mutation guard: process outcomes and journal generation remain independent
// of both the job failure category and provider narrative.
func TestEvidenceTerminalOutcomesAndGeneration(t *testing.T) {
	m, store, id, dir := stageEvidenceJob(t, false)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.Append([]byte(`{"event":"init"}`))
	journal.Close()
	first, err := m.Events(EventsRequest{JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.NextCursor
	oldBytes, err := os.ReadFile(filepath.Join(dir, jobstore.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobstore.WriteProcessDir(dir, jobstore.ProcessOutcome{Source: "supervisor", EndedAt: time.Now(), ExitCode: new(137), TimedOut: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, jobstore.ExitTimeout); err != nil {
		t.Fatal(err)
	}
	page, err := m.Events(EventsRequest{JobID: id})
	if err != nil || page.State != StateFailed || page.Process == nil || !page.Process.TimedOut || *page.Process.ExitCode != 137 || page.AgentOutcome != "unknown" {
		t.Fatalf("timeout evidence lost: %+v %v", page, err)
	}
	newJournal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	newJournal.Close()
	if err := os.WriteFile(filepath.Join(dir, jobstore.EventsFile), oldBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Events(EventsRequest{JobID: id, Cursor: cursor}); err == nil {
		t.Fatal("accepted cursor for restarted journal")
	}
	if m.Collected(id) {
		t.Fatal("event reads collected the result")
	}
}

// Mutation guards: payload omission, malformed-record handling and terminal
// partial-record handling. No lost evidence is presented as a complete event.
func TestEvidenceEventLossIsExplicit(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 1, "step_type": "agent_response", "text_delta": strings.Repeat("x", maxInlineEvent+1)}})
	if err != nil {
		t.Fatal(err)
	}
	journal.Append(raw)
	journal.Close()
	f, err := os.OpenFile(filepath.Join(dir, jobstore.EventsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not JSON\n{\"truncated\":"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	page, err := m.Events(EventsRequest{JobID: id, MaxBytes: 1024})
	if err != nil || len(page.Events) != 1 || !page.Events[0].PayloadOmitted || page.Events[0].Payload != nil || page.SkippedRecords != 2 || !page.IncompleteRecord || page.PendingRecord || page.HasMore {
		t.Fatalf("loss reporting: %+v %v", page, err)
	}
	logs, err := m.Logs(LogRequest{JobID: id, Stream: "events", FromStart: true})
	if err != nil || !strings.Contains(logs.Content, strings.Repeat("x", 100)) {
		t.Fatalf("omitted payload not recoverable through logs: %+v %v", logs, err)
	}
}

// Mutation guards: explicit artifact roots replace the legacy workspace scope,
// and OpenRoot confines reads/listing even when an inner symlink points outside.
func TestEvidenceArtifactScopeAndSymlinks(t *testing.T) {
	m, store, id, dir := stageEvidenceJob(t, false)
	meta, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta.Cwd, "workspace.log"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	external := filepath.Join(outside, "result.log")
	if err := os.WriteFile(external, []byte("external result"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("legacy symlink escapes", func(t *testing.T) {
		link := filepath.Join(meta.Cwd, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := m.ReadArtifact(ReadArtifactRequest{JobID: id, Path: "escape/result.log"}); err == nil {
			t.Fatal("read escaped the workspace through a symlink")
		}
		if _, err := m.ListArtifacts(ListArtifactsRequest{JobID: id, Subpath: "escape"}); err == nil {
			t.Fatal("listing escaped the workspace through a symlink")
		}
	})
	meta.ArtifactDirs = []string{outside}
	meta.Args = append(meta.Args, "--add-dir", meta.Cwd)
	if err := store.UpdateMeta(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadArtifact(ReadArtifactRequest{JobID: id, Path: filepath.Join(meta.Cwd, "workspace.log")}); err == nil {
		t.Fatal("legacy workspace remained authorized despite explicit roots")
	}
	listed, err := m.ListArtifacts(ListArtifactsRequest{JobID: id})
	if err != nil || listed.Scope != "declared" || listed.Root != outside || len(listed.Entries) != 1 || listed.ExpiresAt == "" {
		t.Fatalf("explicit root discovery: %+v %v", listed, err)
	}
	logs, err := m.Logs(LogRequest{JobID: id, Stream: "artifact", Path: external, FromStart: true})
	if err != nil || logs.Content != "external result" || logs.Source != "artifact_file" {
		t.Fatalf("explicit external evidence: %+v %v", logs, err)
	}
	if err := os.WriteFile(jobstore.ErrPath(dir), []byte("cli stderr"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadArtifact(ReadArtifactRequest{JobID: id, Path: "job/err"}); err != nil {
		t.Fatal("job state evidence was excluded", err)
	}
}

// Mutation guard: artifact scope changes affect idempotency identity while
// remaining absent from agy's workspace arguments.
func TestEvidenceArtifactRequestIdentity(t *testing.T) {
	m := newManager(t, managerOpts{})
	base := StartRequest{Prompt: "review", Cwd: t.TempDir()}
	first, err := m.normalizeRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.ArtifactDirs = []string{t.TempDir()}
	next, err := m.normalizeRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	if requestKey(first) == requestKey(next) {
		t.Fatal("changed read authority reused an execution identity")
	}
	if strings.Contains(strings.Join(buildAgyArgs(next), "\x00"), next.ArtifactDirs[0]) {
		t.Fatal("evidence directory was forwarded as an execution workspace")
	}
	if _, err := normalizeArtifactDirs([]string{"relative"}); err == nil {
		t.Fatal("accepted relative artifact authority")
	}
}

// Mutation guards: input limits and absent-journal handling. An old job's
// missing history is unknown, and invalid reads never create/collect work.
func TestEvidenceBoundsAndMissingHistory(t *testing.T) {
	m, store, id, _ := stageEvidenceJob(t, false)
	page, err := m.Events(EventsRequest{JobID: id})
	if err != nil || page.Available || page.AgentOutcome != evidenceOutcomeUnknown || len(page.Events) != 0 || page.Process != nil {
		t.Fatalf("old history was invented: %+v %v", page, err)
	}
	for _, req := range []EventsRequest{{JobID: id, Limit: -1}, {JobID: id, Limit: 201}, {JobID: id, MaxBytes: 512}, {JobID: id, MaxBytes: MaxEvidenceBytes + 1}, {JobID: id, Cursor: "not-a-cursor"}} {
		if _, err := m.Events(req); err == nil {
			t.Errorf("accepted invalid event query %+v", req)
		}
	}
	for _, req := range []LogRequest{{JobID: id, Stream: "invalid"}, {JobID: id, Stream: "stderr", MaxBytes: 3}, {JobID: id, Stream: "stderr", TailBytes: -1}, {JobID: id, Stream: "stderr", FromStart: true, Cursor: "cursor"}, {JobID: id, Stream: "stderr", Path: "unexpected"}} {
		if _, err := m.Logs(req); err == nil {
			t.Errorf("accepted invalid log query %+v", req)
		}
	}
	ids, err := store.List()
	if err != nil || len(ids) != 1 || m.Collected(id) {
		t.Fatalf("read-only boundary changed job store: %v %v", ids, err)
	}
}

// Journal encoding can expand a valid CLI line beyond the reader's record cap.
func TestEvidenceOversizedJournalContinuesPastRecord(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	var legacy bytes.Buffer
	encoder := json.NewEncoder(&legacy) // Legacy journals used HTML escaping.
	for _, raw := range []string{`{"event":"init"}`,
		`{"event":"step_update","detail":"` + strings.Repeat("&", 2<<20) + `"}`,
		`{"event":"result"}`} {
		if err := encoder.Encode(jobstore.JournalRecord{ObservedAt: time.Now(), Event: json.RawMessage(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, jobstore.EventsFile), legacy.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	page, err := m.Events(EventsRequest{JobID: id, MaxBytes: 1024, Limit: 1})
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "init" || !page.HasMore {
		t.Fatalf("initial page = %+v, err = %v", page, err)
	}
	cursor := page.NextCursor
	page, err = m.Events(EventsRequest{JobID: id, Cursor: cursor, MaxBytes: 1024})
	if err != nil || page.SkippedRecords != 1 || page.NextCursor == cursor || !page.HasMore ||
		page.IncompleteRecord || page.PendingRecord || len(page.Events) != 0 {
		t.Fatalf("oversized page = %+v, err = %v", page, err)
	}
	page, err = m.Events(EventsRequest{JobID: id, Cursor: page.NextCursor, MaxBytes: 1024})
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "result" ||
		page.HasMore || page.SkippedRecords != 0 {
		t.Fatalf("terminal page = %+v, err = %v", page, err)
	}
}

func TestEvidenceOmitsPayloadThatExpandsOnWire(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.Append([]byte(`{"event":"step_update","detail":"` + strings.Repeat("&", 300) + `"}`))
	journal.Append([]byte(`{"event":"result"}`))
	journal.Close()

	page, err := m.Events(EventsRequest{JobID: id, MaxBytes: 1024})
	if err != nil || len(page.Events) != 2 || !page.Events[0].PayloadOmitted ||
		page.Events[0].Payload != nil || page.Events[1].Kind != "result" || page.HasMore {
		t.Fatalf("wire expansion blocked the page: %+v, %v", page, err)
	}
}

func TestEvidenceSkipsMetadataLargerThanPage(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.Append([]byte(`{"event":"step_update","step_update":{"step_type":"` + strings.Repeat("x", 1024) + `"}}`))
	journal.Append([]byte(`{"event":"result"}`))
	journal.Close()

	page, err := m.Events(EventsRequest{JobID: id, MaxBytes: 1024})
	if err != nil || page.SkippedRecords != 1 || len(page.Events) != 1 ||
		page.Events[0].Kind != "result" || page.HasMore {
		t.Fatalf("oversized metadata blocked the result event: %+v, %v", page, err)
	}
}

func TestEvidenceBudgetIncludesEventArray(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"event":"step_update","detail":"` + strings.Repeat("x", maxInlineEvent) + `"}`)
	for range 12 {
		journal.Append(payload)
	}
	journal.Close()
	full, err := m.Events(EventsRequest{JobID: id})
	if err != nil || len(full.Events) != 12 {
		t.Fatalf("full page: %d events, %v", len(full.Events), err)
	}
	// Reserve exactly one byte less than all entries as metadata. This
	// distinguishes a budget that includes JSON array punctuation.
	encoded, err := json.Marshal(full.Events)
	if err != nil || len(encoded) <= 1024 {
		t.Fatalf("invalid budget fixture: %d bytes, %v", len(encoded), err)
	}
	limit := len(encoded) - 1
	page, err := m.Events(EventsRequest{JobID: id, MaxBytes: limit})
	if err != nil || !page.HasMore || page.SkippedRecords != 0 {
		t.Fatalf("expected pagination within %d bytes: %+v, %v", limit, page, err)
	}
	pageData, err := json.Marshal(page.Events)
	if err != nil || len(pageData) > limit {
		t.Fatalf("event array exceeded %d bytes: %d, %v", limit, len(pageData), err)
	}
}

func TestEvidenceOversizedJournalKeepsIncompleteLiveCursor(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "append"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			m, store, id, dir := stageEvidenceJob(t, true)
			journal, err := jobstore.OpenJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			journal.Append([]byte(`{"event":"init"}`))
			first, err := m.Events(EventsRequest{JobID: id})
			if err != nil || len(first.Events) != 1 {
				t.Fatalf("initial page = %+v, err = %v", first, err)
			}
			f, err := os.OpenFile(filepath.Join(dir, jobstore.EventsFile), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if _, err := f.WriteString(`{"observed_at":"2026-11-03T00:00:00Z","event":{"event":"step_update","detail":"` +
				strings.Repeat("x", maxJournalRecord)); err != nil {
				t.Fatal(err)
			}

			request := EventsRequest{JobID: id, Cursor: first.NextCursor, MaxBytes: 1024}
			pending, err := m.Events(request)
			if err != nil || pending.NextCursor != request.Cursor || pending.HasMore ||
				!pending.IncompleteRecord || !pending.PendingRecord || pending.SkippedRecords != 0 {
				t.Fatalf("live oversized suffix = %+v, err = %v", pending, err)
			}
			if terminal {
				if err := store.WriteExitCode(id, 0); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.WriteString("\"}}\n"); err != nil {
				t.Fatal(err)
			}

			page, err := m.Events(request)
			if err != nil || page.NextCursor == request.Cursor || page.HasMore ||
				page.PendingRecord || page.SkippedRecords != 1 || page.IncompleteRecord != terminal {
				t.Fatalf("resolved oversized suffix = %+v, err = %v", page, err)
			}
			end, err := m.Events(EventsRequest{JobID: id, Cursor: page.NextCursor})
			if err != nil || end.HasMore || end.SkippedRecords != 0 || end.IncompleteRecord {
				t.Fatalf("suffix was counted again: %+v, err = %v", end, err)
			}
		})
	}
}
