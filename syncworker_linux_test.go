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
		{"qualified 1.5.4", "1.5.4", "cloud sync requires the loaded antigravity-cli-daemon.service user unit", 1},
		{"older", "1.5.3", "found 1.5.3, requires qualified AGY 1.5.4", 78},
		{"newer", "1.5.5", "found 1.5.5, requires qualified AGY 1.5.4", 78},
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
			// A supported version reaches the isolated service check. Avoid
			// querying or starting the host's actual Remote Control unit.
			binDir := filepath.Dir(agy)
			if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' not-found\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "sync-worker")
			cmd.Env = append(cmd.Environ(), "HOME="+profileRoot,
				"AGY_MCP_AGY_PATH="+agy, "AGY_MCP_STATE_DIR="+t.TempDir(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
