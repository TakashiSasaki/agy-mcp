package mcptools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func TestArtifactToolsOverMCP(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	// Create test file
	reportDir := filepath.Join(workspace, "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	samplePath := filepath.Join(reportDir, "summary.json")
	if err := os.WriteFile(samplePath, []byte("{\"test\":\"passed\",\"count\":42}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobID := "mcp-job-artifacts-01"
	store := jobstore.New(stateDir)
	_, err := store.Create(jobstore.Meta{
		ID:  jobID,
		Cwd: workspace,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	mgr := manager.New(config.Config{StateDir: stateDir})

	cs := connect(t, mgr, nil)

	// Test agy_list_artifacts
	resList, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyListArtifacts,
		Arguments: map[string]any{
			"job_id":  jobID,
			"subpath": "reports",
		},
	})
	if err != nil || resList.IsError {
		t.Fatalf("call agy_list_artifacts: err=%v, res=%+v", err, resList)
	}

	scList := structMap(t, resList.StructuredContent)
	entries, ok := scList["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %#v", scList["entries"])
	}
	entryMap, ok := entries[0].(map[string]any)
	if !ok || entryMap["path"] != "summary.json" {
		t.Fatalf("unexpected entry: %#v", entries[0])
	}
	if entryMap["sha256"] == "" {
		t.Error("expected non-empty sha256")
	}

	// Test agy_read_artifact
	resRead, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyReadArtifact,
		Arguments: map[string]any{
			"job_id": jobID,
			"path":   "reports/summary.json",
		},
	})
	if err != nil || resRead.IsError {
		t.Fatalf("call agy_read_artifact: err=%v, res=%+v", err, resRead)
	}

	scRead := structMap(t, resRead.StructuredContent)
	if !strings.Contains(scRead["content"].(string), "\"test\":\"passed\"") {
		t.Errorf("content = %v", scRead["content"])
	}
	if scRead["sha256"] != entryMap["sha256"] {
		t.Errorf("read sha256 %v != list sha256 %v", scRead["sha256"], entryMap["sha256"])
	}
}
