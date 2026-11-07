package mcptools

import (
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// Mutation guard: dropping the alias assignment must lose the original semantic
// code in the wire response, even when error_code itself remains unchanged.
func TestStartErrorAliasPreservesIdempotencyConflictOverMCP(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	state, cwd := t.TempDir(), t.TempDir()
	mgr := manager.New(config.Config{StateDir: state, AgyPath: filepath.Join(t.TempDir(), "unavailable")})
	if _, err := jobstore.New(state).Create(jobstore.Meta{ID: "existing", Cwd: cwd, IdempotencyKey: "retry", RequestKey: "different-request", PID: -1}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mgr, nil)
	for _, name := range []string{toolAgyRun, toolAgyRunSync} {
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"prompt": "changed request", "cwd": cwd, "idempotency_key": "retry"}})
		if err != nil {
			t.Fatal(err)
		}
		out := structMap(t, result.StructuredContent)
		if !result.IsError || out["error_code"] != "idempotency_conflict" || out["start_error_code"] != "idempotency_conflict" || out["job_started"] != false {
			t.Fatalf("original start reason lost: %+v", out)
		}
	}
}
