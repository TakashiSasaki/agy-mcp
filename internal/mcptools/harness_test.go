package mcptools

import (
	"context"
	"encoding/json"
	"log"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// connect exercises the job-supervision fixtures independently of AGY's
// native profile. Cloud admission and sync have separate wire tests.
//
// This lives in an untagged file rather than beside the posix job-supervision
// tests that first used it: nothing in it is platform-specific, and the
// cross-platform tool tests (which must stay green on Windows) need it too.
func connect(t *testing.T, mgr *manager.Manager, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	srv := newServerWithJobStarter(mgr, log.Default(), func(req manager.StartRequest) (manager.Job, error) {
		req.Project, req.Title = "", ""
		return mgr.StartJob(req)
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
	client.AddSendingMiddleware(fixtureProject)
	return connectTestServer(t, srv, client)
}

func fixtureProject(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if params, ok := req.GetParams().(*mcp.CallToolParams); ok &&
			(params.Name == toolAgyRun || params.Name == ToolAgyRunSync) {
			raw, _ := json.Marshal(params.Arguments)
			var args map[string]any
			if json.Unmarshal(raw, &args) == nil && args != nil {
				if _, supplied := args["project"]; !supplied {
					args["project"] = "fixture-project"
					params.Arguments = args
				}
			}
		}
		return next(ctx, method, req)
	}
}

func connectTestServer(t *testing.T, srv *mcp.Server, client *mcp.Client) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Errorf("close client session: %v", err)
		}
	})
	return cs
}
