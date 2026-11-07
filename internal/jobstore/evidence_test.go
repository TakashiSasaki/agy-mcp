package jobstore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadProcessCompletionSpawnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		valid  bool
	}{
		{name: "explicit null", fields: `,"exit_code":null`, valid: true},
		{name: "missing code"},
		{name: "missing process identity", fields: `,"exit_code":127`},
		{name: "unexpected process id", fields: `,"exit_code":null,"pid":42`},
		{name: "unexpected start time", fields: `,"exit_code":null,"started_at":"2026-11-07T01:00:00Z"`},
		{name: "unexpected timeout", fields: `,"exit_code":null,"timed_out":true`},
		{name: "unexpected cancel", fields: `,"exit_code":null,"cancelled":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ProcessFile)
			data := `{"source":"supervisor","ended_at":"2026-11-07T01:02:03Z"` + tc.fields + `}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			stamp := time.Date(2026, 11, 7, 1, 2, 4, 0, time.UTC)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			code, got, ok := ReadProcessCompletion(dir)
			if ok != tc.valid || (ok && (code != ExitSpawnFail || !got.Equal(stamp))) {
				t.Fatalf("completion = (%d, %v, %t), want valid=%t with spawn failure and receipt mtime", code, got, ok, tc.valid)
			}
		})
	}
}

// Mutation guards: journal byte cap, generation publication and malformed-line
// marker. A full spool does not erase earlier evidence or accept partial data.
func TestJournalBoundsAndCompleteness(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.state.MaxBytes = 256
	journal.Append([]byte(`{"event":"init"}`))
	before, err := os.ReadFile(filepath.Join(dir, EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	var record JournalRecord
	if json.Unmarshal(before, &record) != nil || record.ObservedAt.IsZero() {
		t.Fatalf("event not committed before close: %q", before)
	}
	journal.Append([]byte(`{"event":"step_update","text":"` + strings.Repeat("x", 512) + `"}`))
	journal.Close()
	state, err := ReadCaptureDir(dir)
	if err != nil || state == nil || !state.Truncated || !state.Closed || state.DroppedEvents != 1 || state.Generation == "" {
		t.Fatalf("capture loss not explicit: %+v %v", state, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("oversized append corrupted prior records")
	}
	previous := state.Generation
	next, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.NoteMalformed(2)
	next.Close()
	state, err = ReadCaptureDir(dir)
	if err != nil || state.Generation == previous || state.MalformedLines != 2 || !state.Truncated {
		t.Fatalf("new generation or decode loss was hidden: %+v %v", state, err)
	}
}
