package mcptools

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func TestAdoptConversationOverMCP(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY,
		title text NOT NULL DEFAULT "",
		preview text NOT NULL DEFAULT "",
		step_count integer NOT NULL DEFAULT 0,
		workspace_uris text NOT NULL,
		project_id text NOT NULL DEFAULT "",
		status text NOT NULL DEFAULT "",
		not_fully_idle numeric NOT NULL DEFAULT false,
		last_modified_time datetime NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, status, not_fully_idle, last_modified_time)
		VALUES ('conv-mcp-target', '', 'Review API schema compatibility', '', 'default-cli-project', 'COMPLETED', 0, '2026-11-09 10:00:00+00:00')
	`)
	if err != nil {
		t.Fatal(err)
	}
	trajectoryDir := filepath.Join(tempDir, "conversations")
	if err := os.Mkdir(trajectoryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	trajectory, err := sql.Open("sqlite", filepath.Join(trajectoryDir, "conv-mcp-target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = trajectory.Close() }()
	if _, err := trajectory.Exec("CREATE TABLE trajectory_metadata_blob (id text PRIMARY KEY, data blob); INSERT INTO trajectory_metadata_blob VALUES ('main', X'')"); err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(tempDir, "projects")
	_ = os.MkdirAll(projDir, 0o755)
	projJSON := `{"id": "proj-mcp-uuid", "name": "MCP Project", "projectResources": {"resources": [{"folderUri": "file:///data/mcp-project"}]}}`
	_ = os.WriteFile(filepath.Join(projDir, "mcp.json"), []byte(projJSON), 0o644)

	mgr := manager.New(config.Config{StateDir: t.TempDir()})
	mgr.SetProjectDirsForTest(projDir, t.TempDir())
	mgr.SetConversationsDBPathForTest(dbPath)

	cs := connect(t, mgr, nil)

	// Call agy_adopt_conversation over MCP
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyAdoptConversation,
		Arguments: map[string]any{
			"conversation_id": "conv-mcp-target",
			"project":         "MCP Project",
			"title":           "Schema Compatibility Review",
			"restart_daemon":  false, // disable actual systemctl call in test
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call agy_adopt_conversation: err=%v, res=%+v", err, res)
	}

	sc := structMap(t, res.StructuredContent)
	if sc["success"] != true {
		t.Errorf("expected success=true, got %#v", sc)
	}
	if sc["project_id"] != "proj-mcp-uuid" {
		t.Errorf("expected project_id=proj-mcp-uuid, got %v", sc["project_id"])
	}
	if sc["title"] != "Schema Compatibility Review" {
		t.Errorf("expected title='Schema Compatibility Review', got %v", sc["title"])
	}
	if sc["cloud_sync_state"] != "unavailable" || sc["post_restart_verified"] != false {
		t.Errorf("local adoption without daemon evidence was reported verified: %#v", sc)
	}

	// Verify idempotency on second call
	res2, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyAdoptConversation,
		Arguments: map[string]any{
			"conversation_id": "conv-mcp-target",
			"project":         "proj-mcp-uuid",
			"title":           "Schema Compatibility Review",
			"restart_daemon":  false,
		},
	})
	if err != nil || res2.IsError {
		t.Fatalf("second call agy_adopt_conversation: err=%v, res=%+v", err, res2)
	}
	sc2 := structMap(t, res2.StructuredContent)
	if sc2["already_converted"] != true {
		t.Errorf("expected already_converted=true on second call, got %#v", sc2)
	}
}
