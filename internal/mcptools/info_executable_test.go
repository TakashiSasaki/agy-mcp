package mcptools

import (
	"runtime"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Mutation guard: omitting the RuntimeInfo executable fields must fail this
// check through the same tools/call structured result clients consume.
func TestInfoExecutableState(t *testing.T) {
	cs := contractTestClient(t)
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyInfo})
	if err != nil || result.IsError {
		t.Fatalf("info: %+v, %v", result, err)
	}
	out := structuredMap(t, result)
	info, ok := out["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime missing: %+v", out)
	}
	want := "unknown"
	if runtime.GOOS == "linux" {
		want = "current"
	}
	if info["executable_state"] != want || info["executable_advice"] != nil {
		t.Fatalf("unexpected executable diagnostic: %+v", info)
	}
}
