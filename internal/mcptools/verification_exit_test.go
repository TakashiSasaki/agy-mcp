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

// Removing ExitCodePath forwarding must fail the wire verdict assertion.
func TestVerificationCommandExitWire(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	store := jobstore.New(state)
	const id = "command-receipt"
	if _, err := store.Create(jobstore.Meta{ID: id, ArtifactDirs: []string{root}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"tests.tap": "1..1\nok 1 - child\n", "make.exit": "2\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := manager.New(config.Config{StateDir: state, JobTTL: time.Hour})
	cs := connect(t, mgr, nil)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "tests.tap", "format": "tap", "exit_code_path": "make.exit", "max_failures": 0}})
	if err != nil || res.IsError {
		t.Fatalf("verification: %+v %v", res, err)
	}
	out := structuredMap(t, res)
	if out["verification_outcome"] != "failed" || out["report_outcome"] != "passed" || out["passed"] != float64(1) || out["command_exit"].(map[string]any)["exit_code"] != float64(2) {
		t.Fatalf("command failure hidden: %+v", out)
	}
}
