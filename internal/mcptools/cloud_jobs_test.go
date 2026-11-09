package mcptools

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCloudRunRequiresProjectOnProductionServer(t *testing.T) {
	mgr := testManager(t)
	cs := connectTestServer(t, NewServer(mgr), mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil))
	for _, name := range []string{toolAgyRun, ToolAgyRunSync} {
		for _, args := range []map[string]any{{"prompt": "missing"}, {"prompt": "blank", "project": "  "}} {
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil || !result.IsError {
				t.Fatalf("%s accepted missing project: %+v %v", name, result, err)
			}
			out := structMap(t, result.StructuredContent)
			if out["start_error_code"] != startInvalidArgument || out["job_started"] != false {
				t.Fatalf("%s did not refuse before job creation: %+v", name, out)
			}
		}
	}
}
