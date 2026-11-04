package supervisor

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

// Mutation guards: journal.Append in consumeStream and the unconditional step
// heartbeat. Raw unknown fields survive without being invented by the decoder.
func TestSupervisorEvidenceAndHeartbeat(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	step := `{"event":"step_update","step_update":{"step_index":0,"step_type":"tool","state":"ACTIVE","command":"pytest -q"}}`
	consumeStreamObserved(dir, strings.NewReader(step+"\n"+step+"\n"), &bytes.Buffer{}, now)
	progress, ok := jobstore.ReadProgressDir(dir)
	if !ok || progress.StepIndex != 0 || progress.StepState != "ACTIVE" || !progress.UpdatedAt.Equal(time.Date(2026, 11, 3, 0, 0, 2, 0, time.UTC)) {
		t.Fatalf("repeated update did not advance heartbeat: %+v", progress)
	}
	data, err := os.ReadFile(filepath.Join(dir, jobstore.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("journal records = %d, want 2", len(lines))
	}
	var record jobstore.JournalRecord
	if json.Unmarshal(lines[0], &record) != nil || !bytes.Contains(record.Event, []byte(`"command":"pytest -q"`)) {
		t.Fatalf("unknown provider field was discarded: %s", data)
	}
	state, err := jobstore.ReadCaptureDir(dir)
	if err != nil || state == nil || !state.Closed || state.Truncated {
		t.Fatalf("capture state: %+v %v", state, err)
	}
}
