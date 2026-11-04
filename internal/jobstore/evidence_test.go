package jobstore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
