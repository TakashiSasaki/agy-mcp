package jobstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const MaxEventJournalBytes int64 = 64 << 20

// Capture describes evidence capture, independently of agent/task success.
type Capture struct {
	Generation     string `json:"generation" jsonschema:"unique journal generation; changes if capture is restarted"`
	MaxBytes       int64  `json:"max_bytes" jsonschema:"journal storage limit in bytes"`
	Closed         bool   `json:"closed" jsonschema:"the supervisor closed this capture; this is not a task-success verdict"`
	Truncated      bool   `json:"truncated" jsonschema:"some events could not be retained because of a limit or write failure"`
	DroppedEvents  int64  `json:"dropped_events" jsonschema:"events observed but not retained in this journal"`
	MalformedLines int    `json:"malformed_lines" jsonschema:"unreadable CLI lines omitted by the decoder; their event count is unknown"`
	Error          string `json:"error,omitempty" jsonschema:"capture failure code; absent for an otherwise healthy capture"`
}

type JournalRecord struct {
	ObservedAt time.Time       `json:"observed_at"`
	Event      json.RawMessage `json:"event"`
}

// ProcessOutcome describes agy, never a shell command launched by the model.
type ProcessOutcome struct {
	Source    string     `json:"source" jsonschema:"supervisor; observes the agy CLI process, not individual model tool commands"`
	PID       int        `json:"pid,omitempty" jsonschema:"agy process ID when it was started"`
	StartedAt *time.Time `json:"started_at,omitempty" jsonschema:"time the supervisor started agy; absent when spawning failed"`
	EndedAt   time.Time  `json:"ended_at" jsonschema:"time the supervisor reaped agy or observed spawn failure"`
	ExitCode  *int       `json:"exit_code" jsonschema:"observed agy exit status with shell-style signal codes; null if spawning failed"`
	TimedOut  bool       `json:"timed_out" jsonschema:"the supervisor deadline caused process termination"`
	Cancelled bool       `json:"cancelled" jsonschema:"the supervisor cancellation caused process termination"`
}

func WriteProcessDir(dir string, outcome ProcessOutcome) error {
	data, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, ProcessFile, data)
}

func ReadProcessDir(dir string) (*ProcessOutcome, error) {
	data, err := readReplaced(filepath.Join(dir, ProcessFile))
	if err != nil {
		return nil, err
	}
	var outcome ProcessOutcome
	if err := json.Unmarshal(data, &outcome); err != nil {
		return nil, err
	}
	return &outcome, nil
}

func ReadCaptureDir(dir string) (*Capture, error) {
	data, err := readReplaced(filepath.Join(dir, CaptureFile))
	if err != nil {
		return nil, err
	}
	var capture Capture
	if err := json.Unmarshal(data, &capture); err != nil {
		return nil, err
	}
	if capture.Generation == "" {
		return nil, &os.PathError{Op: "read", Path: CaptureFile, Err: os.ErrInvalid}
	}
	return &capture, nil
}

type Journal struct {
	dir   string
	file  *os.File
	bytes int64
	state Capture
}

// OpenJournal publishes its generation before accepting event bytes. Writes are
// unbuffered, so hard termination retains every completed append.
func OpenJournal(dir string) (*Journal, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, state: Capture{Generation: hex.EncodeToString(id[:]), MaxBytes: MaxEventJournalBytes}}
	f, err := os.OpenFile(filepath.Join(dir, EventsFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	j.file = f
	if err := j.persist(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return j, nil
}

func (j *Journal) persist() error {
	data, err := json.Marshal(j.state)
	if err != nil {
		return err
	}
	return writeFileAtomic(j.dir, CaptureFile, data)
}

// Append keeps the provider event without treating narrative as a command result.
// Capture failure never interrupts the independent result decoder.
func (j *Journal) Append(raw []byte) {
	if j.state.Truncated {
		j.state.DroppedEvents++
		return
	}
	data, err := json.Marshal(JournalRecord{ObservedAt: time.Now().UTC(), Event: json.RawMessage(raw)})
	if err == nil && j.bytes+int64(len(data)+1) > j.state.MaxBytes {
		err = io.ErrShortBuffer
	}
	if err == nil {
		var n int
		n, err = j.file.Write(append(data, '\n'))
		j.bytes += int64(n)
		if err == nil && n != len(data)+1 {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		j.state.Truncated = true
		j.state.DroppedEvents++
		j.state.Error = "capture_write_failed"
		if errors.Is(err, io.ErrShortBuffer) {
			j.state.Error = "capture_limit_reached"
		}
		_ = j.persist()
	}
}

func (j *Journal) Close() {
	j.state.Closed = true
	if err := j.file.Close(); err != nil {
		j.state.Truncated = true
		j.state.Error = "capture_close_failed"
	}
	_ = j.persist()
}

func (j *Journal) NoteMalformed(lines int) {
	if lines == 0 {
		return
	}
	j.state.MalformedLines = lines
	j.state.Truncated = true
	if j.state.Error == "" {
		j.state.Error = "malformed_cli_lines"
	}
}
