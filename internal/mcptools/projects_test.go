package mcptools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func TestProjectsToolsOverMCP(t *testing.T) {
	tempBase := filepath.Join(os.TempDir(), "agy-test-mcp-projects")
	if err := os.MkdirAll(tempBase, 0o755); err != nil {
		t.Fatalf("mkdir tempBase: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempBase) })

	testProjectsDir := filepath.Join(tempBase, "projects")
	testDataDir := filepath.Join(tempBase, "data")
	_ = os.MkdirAll(testProjectsDir, 0o755)
	_ = os.MkdirAll(testDataDir, 0o755)

	sampleProjectDir := filepath.Join(tempBase, "sample-repo")
	_ = os.MkdirAll(sampleProjectDir, 0o755)

	projConfig := map[string]any{
		"id":   "sample-app",
		"name": "Sample App",
		"projectResources": map[string]any{
			"resources": []map[string]any{
				{
					"folderUri": "file://" + sampleProjectDir,
					"gitFolder": map[string]any{
						"folderUri":     "file://" + sampleProjectDir,
						"defaultBranch": "main",
					},
				},
			},
		},
	}
	b, err := json.Marshal(projConfig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = os.WriteFile(filepath.Join(testProjectsDir, "sample.json"), b, 0o644)

	mgr := manager.New(config.Config{StateDir: t.TempDir()})
	mgr.SetProjectDirsForTest(testProjectsDir, testDataDir)

	cs := connect(t, mgr, nil)

	// 1. Call agy_list_projects
	resList, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyListProjects,
		Arguments: map[string]any{
			"query": "Sample",
		},
	})
	if err != nil || resList.IsError {
		t.Fatalf("call agy_list_projects: err=%v, res=%+v", err, resList)
	}
	scList := structMap(t, resList.StructuredContent)
	projs, ok := scList["projects"].([]any)
	if !ok || len(projs) != 1 {
		t.Fatalf("expected 1 project, got %#v", scList["projects"])
	}
	p0 := projs[0].(map[string]any)
	if p0["name"] != "Sample App" {
		t.Errorf("expected Sample App, got %v", p0["name"])
	}

	// 2. Call agy_resolve_workspace
	resResolve, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyResolveWorkspace,
		Arguments: map[string]any{
			"project": "sample-app",
		},
	})
	if err != nil || resResolve.IsError {
		t.Fatalf("call agy_resolve_workspace: err=%v, res=%+v", err, resResolve)
	}
	scResolve := structMap(t, resResolve.StructuredContent)
	if scResolve["resolved"] != true {
		t.Errorf("expected resolved=true, got %#v", scResolve)
	}
	if scResolve["path"] != sampleProjectDir {
		t.Errorf("expected path=%s, got %v", sampleProjectDir, scResolve["path"])
	}
}
