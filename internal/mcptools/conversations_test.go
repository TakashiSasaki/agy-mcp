package mcptools

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func createTestConversationsDBForMCP(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY,
		title text NOT NULL DEFAULT "",
		preview text NOT NULL DEFAULT "",
		step_count integer NOT NULL DEFAULT 0,
		last_modified_time datetime NOT NULL,
		workspace_uris text NOT NULL,
		status text NOT NULL DEFAULT "",
		source text NOT NULL DEFAULT "",
		project_id text NOT NULL DEFAULT "",
		agent_name text NOT NULL DEFAULT ""
	);
	CREATE VIRTUAL TABLE conversations_fts USING fts5(
		conversation_id UNINDEXED,
		archived UNINDEXED,
		title
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("exec schema: %v", err)
	}

	insertSQL := `
	INSERT INTO conversation_summaries (conversation_id, title, step_count, last_modified_time, workspace_uris, status, project_id)
	VALUES 
		('conv-1', 'Active Session', 8, '2026-11-09 08:00:00+00:00', '["file:///data/repo1"]', 'COMPLETED', 'proj-1'),
		('conv-2', 'Archived Session', 3, '2026-11-09 07:00:00+00:00', '["file:///data/repo1"]', 'COMPLETED', 'proj-1');
	`
	if _, err := db.Exec(insertSQL); err != nil {
		t.Fatalf("insert summaries: %v", err)
	}

	insertFTS := `
	INSERT INTO conversations_fts (conversation_id, archived, title)
	VALUES
		('conv-1', 0, 'Active Session'),
		('conv-2', 1, 'Archived Session');
	`
	if _, err := db.Exec(insertFTS); err != nil {
		t.Fatalf("insert fts: %v", err)
	}

	return dbPath
}

func TestConversationsToolsOverMCP(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := createTestConversationsDBForMCP(t, tempDir)

	mgr := manager.New(config.Config{StateDir: t.TempDir()})
	mgr.SetConversationsDBPathForTest(dbPath)

	cs := connect(t, mgr, nil)

	// 1. Call agy_list_conversations (default: exclude archived)
	resList, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyListConversations,
		Arguments: map[string]any{
			"project_id": "proj-1",
		},
	})
	if err != nil || resList.IsError {
		t.Fatalf("call agy_list_conversations: err=%v, res=%+v", err, resList)
	}
	sc := structMap(t, resList.StructuredContent)
	convs, ok := sc["conversations"].([]any)
	if !ok || len(convs) != 1 {
		t.Fatalf("expected 1 active conversation, got %#v", sc["conversations"])
	}
	c0 := convs[0].(map[string]any)
	if c0["conversation_id"] != "conv-1" {
		t.Errorf("expected conv-1, got %v", c0["conversation_id"])
	}
	if c0["archived"] != false {
		t.Errorf("expected archived=false, got %v", c0["archived"])
	}

	// 2. Call agy_list_conversations with include_archived=true
	resAll, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: toolAgyListConversations,
		Arguments: map[string]any{
			"include_archived": true,
		},
	})
	if err != nil || resAll.IsError {
		t.Fatalf("call agy_list_conversations include_archived: err=%v, res=%+v", err, resAll)
	}
	scAll := structMap(t, resAll.StructuredContent)
	convsAll, ok := scAll["conversations"].([]any)
	if !ok || len(convsAll) != 2 {
		t.Fatalf("expected 2 conversations, got %#v", scAll["conversations"])
	}
}
