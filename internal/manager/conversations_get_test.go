package manager

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestGetConversation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY,
		title text NOT NULL DEFAULT "",
		step_count integer NOT NULL DEFAULT 0,
		last_modified_time datetime NOT NULL,
		workspace_uris text NOT NULL,
		status text NOT NULL DEFAULT "",
		project_id text NOT NULL DEFAULT ""
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, step_count, last_modified_time, workspace_uris, status, project_id)
		VALUES ('conv-get-123', 'My Session', 42, '2026-11-09 12:00:00+00:00', '["file:///data/ws"]', 'COMPLETED', 'default-cli-project')
	`)
	if err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		conversationsDBPath: dbPath,
	}

	item, err := mgr.GetConversation("conv-get-123")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if item.ConversationID != "conv-get-123" {
		t.Errorf("ConversationID = %s, want conv-get-123", item.ConversationID)
	}
	if item.Title != "My Session" {
		t.Errorf("Title = %s, want 'My Session'", item.Title)
	}
	if item.StepCount != 42 {
		t.Errorf("StepCount = %d, want 42", item.StepCount)
	}

	// Non-existent conversation
	_, err = mgr.GetConversation("conv-does-not-exist")
	if err == nil {
		t.Errorf("expected error for non-existent conversation")
	}
}
