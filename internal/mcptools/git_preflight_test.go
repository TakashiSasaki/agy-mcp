package mcptools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInputForwardsGitPreconditions(t *testing.T) {
	in := runInput{Prompt: "review", ExpectedGitBranch: "main", ExpectedGitHead: "0123456789abcdef0123456789abcdef01234567"}
	req, err := in.toStartRequest()
	if err != nil || req.ExpectedGitBranch != in.ExpectedGitBranch || req.ExpectedGitHead != in.ExpectedGitHead {
		t.Fatalf("preconditions dropped: %+v %v", req, err)
	}
	for _, field := range []string{"expected_git_head", "expected_git_branch"} {
		if runInputSchema[runInput]().Properties[field] == nil {
			t.Errorf("async schema missing %s", field)
		}
		if runInputSchema[runSyncInput]().Properties[field] == nil {
			t.Errorf("sync schema missing %s", field)
		}
	}
}

// Mutation guard: the MCP projection must expose full IDs and both run tools
// must enforce forwarded preconditions before trying to execute an agent.
func TestGitPreconditionsAndFullHeadOverMCP(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
	mgr := manager.New(config.Config{StateDir: t.TempDir(), AgyPath: filepath.Join(t.TempDir(), "missing-agy")})
	cs := connect(t, mgr, nil)
	resolved, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyResolveWorkspace, Arguments: map[string]any{"project": repo, "branch": "main"}})
	if err != nil || resolved.IsError {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	data := structMap(t, resolved.StructuredContent)
	if data["head_full"] != head || data["head"] == head || data["resolved"] != true {
		t.Fatalf("full ID projection lost or short compatibility changed: %+v", data)
	}
	if !proc.Supported {
		return
	} // Full-head discovery works even where supervision is unavailable.
	for _, name := range []string{toolAgyRun, toolAgyRunSync} {
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"prompt": "report", "cwd": repo, "expected_git_branch": "wrong", "expected_git_head": head}})
		if err != nil {
			t.Fatal(err)
		}
		d := structMap(t, r.StructuredContent)
		if !r.IsError || d["error_code"] != "workspace_mismatch" || d["job_started"] != false {
			t.Fatalf("%s bypassed precondition: %+v", name, d)
		}
	}
}
