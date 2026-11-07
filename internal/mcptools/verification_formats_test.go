package mcptools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Mutation guards: the explicit format must reach the parser, preserve evidence
// identity and retain package failure even when details are suppressed.
func TestVerificationGoFormat(t *testing.T) {
	state, reports := t.TempDir(), t.TempDir()
	store := jobstore.New(state)
	const id = "go-report"
	if _, err := store.Create(jobstore.Meta{ID: id, ArtifactDirs: []string{reports}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	report := "{\"Action\":\"start\",\"Package\":\"example\"}\n{\"Action\":\"run\",\"Package\":\"example\",\"Test\":\"TestOK\"}\n{\"Action\":\"pass\",\"Package\":\"example\",\"Test\":\"TestOK\"}\n{\"Action\":\"fail\",\"Package\":\"example\"}\n"
	if err := os.WriteFile(filepath.Join(reports, "tests.jsonl"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(config.Config{StateDir: state, JobTTL: time.Hour})
	cs := connect(t, mgr, nil)
	for _, format := range []string{"go_test_json", "junit", "invalid"} {
		t.Run(format, func(t *testing.T) {
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "tests.jsonl", "format": format, "max_failures": 0}})
			if format == "invalid" {
				if err == nil && !res.IsError {
					t.Fatal("invalid format accepted")
				}
				return
			}
			if err != nil || res.IsError {
				t.Fatalf("verification: %+v %v", res, err)
			}
			out := structuredMap(t, res)
			if format == "junit" {
				if out["verification_outcome"] != "unknown" {
					t.Fatalf("format was guessed: %+v", out)
				}
				return
			}
			if out["source"] != "go_test_json_artifact" || out["verification_outcome"] != "failed" || out["complete"] != true || out["passed"] != float64(1) || out["package_failures"] != float64(1) || len(out["failures"].([]any)) != 0 || out["failures_truncated"] != true {
				t.Fatalf("format lost package failure: %+v", out)
			}
		})
	}
	if mgr.Collected(id) {
		t.Fatal("verification collected agent result")
	}
}

// Removing the TAP dispatch must fail this test. Suppressing details must not
// hide a failed case or turn an incomplete report into a pass.
func TestVerificationTAPFormat(t *testing.T) {
	state, reports := t.TempDir(), t.TempDir()
	store := jobstore.New(state)
	const id = "tap-report"
	if _, err := store.Create(jobstore.Meta{ID: id, ArtifactDirs: []string{reports}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(config.Config{StateDir: state, JobTTL: time.Hour})
	cs := connect(t, mgr, nil)
	for _, tc := range []struct{ name, report, outcome string }{
		{"pass", "1..1\nok 1 - success\n", "passed"},
		{"fail", "1..2\nok 1 - success\nnot ok 2 - failure\n", "failed"},
		{"truncated", "1..2\nok 1 - success\n", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(reports, "report.tap"), []byte(tc.report), 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "report.tap", "format": "tap", "max_failures": 0}})
			if err != nil || res.IsError {
				t.Fatalf("verification: %+v %v", res, err)
			}
			out := structuredMap(t, res)
			if out["source"] != "tap_artifact" || out["verification_outcome"] != tc.outcome || len(out["failures"].([]any)) != 0 {
				t.Fatalf("TAP verdict lost: %+v", out)
			}
			if tc.outcome == "failed" && (out["failed"] != float64(1) || out["failures_truncated"] != true) {
				t.Fatalf("summary hid failure: %+v", out)
			}
		})
	}
}
