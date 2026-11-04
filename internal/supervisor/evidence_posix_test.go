//go:build linux || darwin

package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Mutation guards: process receipt publication before the sentinel and live
// event capture. Real timeout/group termination must retain already-read events.
func TestSupervisorTimeoutRetainsEvidence(t *testing.T) {
	agy := filepath.Join(t.TempDir(), "fake-agy")
	script := "#!/bin/sh\ntrap '' TERM\nprintf '%s\\n' '{\"event\":\"step_update\",\"step_update\":{\"step_index\":0,\"step_type\":\"tool\",\"state\":\"ACTIVE\"}}'\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(agy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := jobstore.New(t.TempDir())
	dir, err := store.Create(jobstore.Meta{ID: "timeout-evidence", AgyPath: agy, Cwd: t.TempDir(), Args: []string{"--output-format", "stream-json"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(dir, 20*time.Millisecond, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	code, ok := store.ExitCode("timeout-evidence")
	if !ok || code != jobstore.ExitTimeout {
		t.Fatalf("timeout sentinel: %d %t", code, ok)
	}
	receipt, err := jobstore.ReadProcessDir(dir)
	if err != nil || receipt == nil || !receipt.TimedOut || receipt.ExitCode == nil || receipt.StartedAt == nil || receipt.EndedAt.IsZero() {
		t.Fatalf("process evidence absent: %+v %v", receipt, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, jobstore.EventsFile))
	if err != nil || !strings.Contains(string(data), "step_update") {
		t.Fatalf("timed-out tool progress disappeared: %q %v", data, err)
	}
	capture, err := jobstore.ReadCaptureDir(dir)
	if err != nil || capture == nil || !capture.Closed {
		t.Fatalf("event drain did not close: %+v %v", capture, err)
	}
}
