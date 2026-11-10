package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// Without the ErrCloudAGYVersion branch in main, systemd would retry the
// unqualified binary just as it would retry a failed probe.
func TestSyncWorkerVersionExitStatus(t *testing.T) {
	t.Parallel()
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version, diagnostic string
		exit                      int
	}{
		{"unqualified", "9.8.7", "found 9.8.7, requires qualified AGY 1.5.3", 78},
		{"unreadable", "private-probe-banner", "cannot verify AGY version", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			profileRoot := t.TempDir()
			profile := filepath.Join(profileRoot, ".gemini", "antigravity-cli")
			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, "conversation_summaries.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Version: tc.version})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "sync-worker")
			cmd.Env = append(cmd.Environ(), "HOME="+profileRoot,
				"AGY_MCP_AGY_PATH="+agy, "AGY_MCP_STATE_DIR="+t.TempDir())
			output, err := cmd.CombinedOutput()
			exit, failed := errors.AsType[*exec.ExitError](err)
			if !failed || exit.ExitCode() != tc.exit || !strings.Contains(string(output), tc.diagnostic) {
				t.Fatalf("wanted exit %d and %q, got %v: %s", tc.exit, tc.diagnostic, err, output)
			}
			if strings.Contains(string(output), "private-probe-banner") {
				t.Fatalf("version probe output entered the worker log: %s", output)
			}
		})
	}
}
