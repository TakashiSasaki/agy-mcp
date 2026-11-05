//go:build linux || darwin

package manager

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
)

// Mutation guards: RuntimeInfo's checkAuth condition, effective defaults and
// process-local gate counters. The fake accepts only version/model listing.
func TestAgentRuntimeInfoAuthProbeIsExplicit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "models-probed")
	t.Setenv("AGY_INFO_PROBE_FILE", marker)
	agy := writeProbeScript(t, "models", "printf checked > \"$AGY_INFO_PROBE_FILE\"\necho '{\"status\":\"SUCCESS\",\"command\":{\"name\":\"models\",\"data\":{\"models\":[]}}}'\nexit 0\n")
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), DefaultModel: "gemini-test\tTest", DefaultTimeout: 30 * time.Minute, MaxConcurrency: 2, JobTTL: 24 * time.Hour})
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.gate.inFlight = 1
	info, err := m.RuntimeInfo(t.Context(), false)
	if err != nil || info.AuthStatus != "not_checked" || info.AuthCheckedAt != "" || info.AgyStatus != "ok" || info.ReadOnlyEnforced || info.RunningJobs != 1 || info.MaxConcurrency != 2 || info.DefaultModel != "gemini-test" || info.DefaultTimeout != "30m0s" || info.JobTTL != "24h0m0s" || info.DefaultCwd == "" {
		t.Fatalf("runtime defaults/checks: %+v, %v", info, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default info unexpectedly probed models: %v", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if info.Hostname != hostname || info.OS != runtime.GOOS || info.Architecture != runtime.GOARCH {
		t.Fatalf("wrong server identity: %+v", info)
	}
	info, err = m.RuntimeInfo(t.Context(), true)
	if err != nil || info.AuthStatus != "ok" || info.AuthCheckedAt != "2026-11-03T00:00:00Z" {
		t.Fatalf("explicit model-list probe: %+v, %v", info, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "checked" {
		t.Fatalf("explicit probe was not made: %q, %v", data, err)
	}
	ids, err := m.store.List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("diagnosis created jobs: %v, %v", ids, err)
	}
}
