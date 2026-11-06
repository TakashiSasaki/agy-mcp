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
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// Mutation guards: cache-resolution errors must be returned before creating a
// new job, and the start-tool adapter must retain the structured refusal.
func TestRunRefusesUnreadableConversationCache(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	for _, body := range []string{`{"torn`, `null`, `[]`} {
		t.Run(body, func(t *testing.T) {
			state := t.TempDir()
			cache := filepath.Join(t.TempDir(), "cache.json")
			if err := os.WriteFile(cache, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			mgr := manager.New(config.Config{StateDir: state, ConversationCacheFile: cache, AgyPath: filepath.Join(t.TempDir(), "missing-agy"), DefaultTimeout: time.Minute, MaxConcurrency: 1})
			cs := connect(t, mgr, nil)
			for _, name := range []string{toolAgyRun, toolAgyRunSync} {
				result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"prompt": "continue the prior task", "cwd": t.TempDir(), "continue_latest": true, "idempotency_key": "retry-cache"}})
				if err != nil || !result.IsError {
					t.Fatalf("wanted a cache refusal: %+v %v", result, err)
				}
				out := structuredMap(t, result)
				if out["error_code"] != "conversation_cache_unavailable" || out["job_started"] != false || out["retryable"] != true || out["next_action"] != "retry_same_request" {
					t.Errorf("cache failure must not fall back to a fresh run: %+v", out)
				}
			}
			ids, err := jobstore.New(state).List()
			if err != nil || len(ids) != 0 {
				t.Fatalf("cache failure created jobs: %v %v", ids, err)
			}
		})
	}
}
