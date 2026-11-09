package manager

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func createTestConversationsDB(t *testing.T, dir string) string {
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

	// Insert test conversations
	insertSQL := `
	INSERT INTO conversation_summaries (conversation_id, title, step_count, last_modified_time, workspace_uris, status, project_id)
	VALUES 
		('c1', 'Active Cloud Conversation', 10, '2026-11-09 08:00:00+00:00', '["file:///data/repo1"]', 'COMPLETED', 'proj-uuid-1'),
		('c2', 'Archived Cloud Conversation', 5, '2026-11-09 07:00:00+00:00', '["file:///data/repo1"]', 'COMPLETED', 'proj-uuid-1'),
		('c3', 'Local CLI Conversation', 2, '2026-11-09 06:00:00+00:00', '["file:///data/repo2"]', 'RUNNING', 'default-cli-project'),
		('c4', 'Outside Project Task', 1, '2026-11-09 05:00:00+00:00', '', 'FAILED', 'outside-of-project');
	`
	if _, err := db.Exec(insertSQL); err != nil {
		t.Fatalf("insert summaries: %v", err)
	}

	// Insert FTS rows: c2 is archived (1)
	insertFTS := `
	INSERT INTO conversations_fts (conversation_id, archived, title)
	VALUES
		('c1', 0, 'Active Cloud Conversation'),
		('c2', 1, 'Archived Cloud Conversation'),
		('c3', 0, 'Local CLI Conversation'),
		('c4', 0, 'Outside Project Task');
	`
	if _, err := db.Exec(insertFTS); err != nil {
		t.Fatalf("insert fts: %v", err)
	}

	return dbPath
}

func TestListConversationsFilteringAndPagination(t *testing.T) {
	dir := t.TempDir()
	dbPath := createTestConversationsDB(t, dir)

	// Create a mock registered project for proj-uuid-1
	projDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{
		"id": "proj-uuid-1",
		"name": "Cloud Project",
		"projectResources": {"resources": [{"folderUri": "file:///data/repo1"}]},
		"isWorkspaceOnly": false
	}`
	if err := os.WriteFile(filepath.Join(projDir, "proj-uuid-1.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	// 1. Default list: should exclude archived (c2), returning c1, c3, c4
	res, err := mgr.ListConversations(ListConversationsInput{IncludeArchived: false})
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if res.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", res.TotalCount)
	}
	if len(res.Conversations) != 3 {
		t.Fatalf("len(Conversations) = %d, want 3", len(res.Conversations))
	}
	// Check c1 has IsCloudSynced = true
	if res.Conversations[0].ConversationID != "c1" || !res.Conversations[0].IsCloudSynced {
		t.Errorf("expected c1 to be cloud synced, got %+v", res.Conversations[0])
	}
	// Check c3 has IsCloudSynced = false
	if res.Conversations[1].ConversationID != "c3" || res.Conversations[1].IsCloudSynced {
		t.Errorf("expected c3 to not be cloud synced, got %+v", res.Conversations[1])
	}

	// 2. Include archived: should return all 4
	resArchived, err := mgr.ListConversations(ListConversationsInput{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListConversations include archived: %v", err)
	}
	if resArchived.TotalCount != 4 {
		t.Errorf("TotalCount with archived = %d, want 4", resArchived.TotalCount)
	}

	// 3. Filter by project_id = "proj-uuid-1" with archived included
	resProj, err := mgr.ListConversations(ListConversationsInput{
		ProjectID:       "proj-uuid-1",
		IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("ListConversations by project: %v", err)
	}
	if resProj.TotalCount != 2 {
		t.Errorf("TotalCount for proj-uuid-1 = %d, want 2", resProj.TotalCount)
	}

	// 4. Query search substring
	resQuery, err := mgr.ListConversations(ListConversationsInput{
		Query:           "CLI",
		IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("ListConversations by query: %v", err)
	}
	if resQuery.TotalCount != 1 || resQuery.Conversations[0].ConversationID != "c3" {
		t.Errorf("unexpected query result: %+v", resQuery)
	}

	// 5. Pagination: limit 1, offset 1
	resPage, err := mgr.ListConversations(ListConversationsInput{
		IncludeArchived: true,
		Limit:           1,
		Offset:          1,
	})
	if err != nil {
		t.Fatalf("ListConversations pagination: %v", err)
	}
	if len(resPage.Conversations) != 1 || resPage.Conversations[0].ConversationID != "c2" {
		t.Errorf("unexpected paged item: %+v", resPage.Conversations)
	}
	if !resPage.HasMore {
		t.Errorf("expected has_more to be true")
	}
}

func TestGetProjectConversationStats(t *testing.T) {
	dir := t.TempDir()
	dbPath := createTestConversationsDB(t, dir)

	mgr := &Manager{
		conversationsDBPath: dbPath,
	}

	stats, err := mgr.GetProjectConversationStats()
	if err != nil {
		t.Fatalf("GetProjectConversationStats: %v", err)
	}

	proj1 := stats["proj-uuid-1"]
	if proj1.Count != 2 {
		t.Errorf("proj-uuid-1 count = %d, want 2", proj1.Count)
	}
	if proj1.LastActive == "" {
		t.Errorf("proj-uuid-1 last active is empty")
	}

	cli := stats["default-cli-project"]
	if cli.Count != 1 {
		t.Errorf("default-cli-project count = %d, want 1", cli.Count)
	}
}
