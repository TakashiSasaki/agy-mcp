package manager

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAdoptConversationSuccessAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
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
		agent_name text NOT NULL DEFAULT "",
		not_fully_idle numeric NOT NULL DEFAULT false
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	// Insert orphan session without title or project
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, not_fully_idle, last_modified_time)
		VALUES ('conv-target-1', '', 'How do I optimize this Go codebase for concurrency?', '', 'default-cli-project', 0, '2026-11-09 10:00:00+00:00')
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Create registered project
	projDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{
		"id": "proj-uuid-1234",
		"name": "Target App",
		"projectResources": {"resources": [{"folderUri": "file:///data/target-app"}]},
		"isWorkspaceOnly": false
	}`
	if err := os.WriteFile(filepath.Join(projDir, "target.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	var restartCalls int32
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error {
		atomic.AddInt32(&restartCalls, 1)
		return nil
	})

	ctx := t.Context()

	// 1. First adoption: should update DB and restart daemon
	res1, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "Target App", // resolve by name
		Title:          "Concurrency Optimization",
	})
	if err != nil {
		t.Fatalf("first AdoptConversation: %v", err)
	}
	if !res1.Success {
		t.Errorf("expected success=true")
	}
	if res1.AlreadyConverted {
		t.Errorf("expected already_converted=false on first run")
	}
	if res1.ProjectID != "proj-uuid-1234" {
		t.Errorf("expected ProjectID=proj-uuid-1234, got %s", res1.ProjectID)
	}
	if res1.Title != "Concurrency Optimization" {
		t.Errorf("expected Title='Concurrency Optimization', got %s", res1.Title)
	}
	if !res1.DaemonRestarted {
		t.Errorf("expected daemon_restarted=true, got reason: %s", res1.DaemonRestartReason)
	}
	if atomic.LoadInt32(&restartCalls) != 1 {
		t.Errorf("expected 1 restart call, got %d", atomic.LoadInt32(&restartCalls))
	}

	// Verify DB state
	var dbPID, dbTitle, dbURI string
	_ = db.QueryRow("SELECT project_id, title, workspace_uris FROM conversation_summaries WHERE conversation_id = 'conv-target-1'").Scan(&dbPID, &dbTitle, &dbURI)
	if dbPID != "proj-uuid-1234" || dbTitle != "Concurrency Optimization" {
		t.Errorf("DB state unexpected: pid=%s, title=%s", dbPID, dbTitle)
	}

	// 2. Second adoption with same arguments: should be IDEMPOTENT (already_converted=true, cooldown skips restart)
	res2, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "proj-uuid-1234", // resolve by ID
		Title:          "Concurrency Optimization",
	})
	if err != nil {
		t.Fatalf("second AdoptConversation: %v", err)
	}
	if !res2.AlreadyConverted {
		t.Errorf("expected already_converted=true on second run")
	}
	if res2.DaemonRestarted {
		t.Errorf("expected cooldown to prevent second restart")
	}
	if atomic.LoadInt32(&restartCalls) != 1 {
		t.Errorf("restart call count should remain 1, got %d", atomic.LoadInt32(&restartCalls))
	}
}

func TestAdoptConversationTitleDerivationFromPreview(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY,
		title text NOT NULL DEFAULT "",
		preview text NOT NULL DEFAULT "",
		workspace_uris text NOT NULL,
		project_id text NOT NULL DEFAULT "",
		not_fully_idle numeric NOT NULL DEFAULT false,
		last_modified_time datetime NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, not_fully_idle, last_modified_time)
		VALUES ('conv-preview-test', '', 'Analyze PostgreSQL query latency and generate indexing recommendations', '', 'default-cli-project', 0, '2026-11-09 10:00:00+00:00')
	`)
	if err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(dir, "projects")
	_ = os.MkdirAll(projDir, 0o755)
	projJSON := `{"id": "proj-db", "name": "DB Project", "projectResources": {"resources": [{"folderUri": "file:///data/db"}]}}`
	_ = os.WriteFile(filepath.Join(projDir, "db.json"), []byte(projJSON), 0o644)

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error { return nil })

	// Adopt without Title specified
	res, err := mgr.AdoptConversation(t.Context(), AdoptConversationInput{
		ConversationID: "conv-preview-test",
		Project:        "DB Project",
	})
	if err != nil {
		t.Fatalf("AdoptConversation: %v", err)
	}
	if res.Title != "Analyze PostgreSQL query latency and generate indexing recom..." {
		t.Errorf("unexpected derived title: %q", res.Title)
	}
}

func TestAdoptConversationGracefulRestartIdleCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY,
		title text NOT NULL DEFAULT "",
		preview text NOT NULL DEFAULT "",
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

	// Insert an active running conversation (not idle!)
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, status, not_fully_idle, last_modified_time)
		VALUES 
			('conv-active', 'Active Task', '', '', 'proj-1', 'CASCADE_RUN_STATUS_RUNNING', 1, '2026-11-09 10:00:00+00:00'),
			('conv-target', '', 'Some prompt', '', 'default-cli-project', 'COMPLETED', 0, '2026-11-09 09:00:00+00:00')
	`)
	if err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(dir, "projects")
	_ = os.MkdirAll(projDir, 0o755)
	projJSON := `{"id": "proj-1", "name": "App", "projectResources": {"resources": [{"folderUri": "file:///data/app"}]}}`
	_ = os.WriteFile(filepath.Join(projDir, "app.json"), []byte(projJSON), 0o644)

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	var restarted bool
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error {
		restarted = true
		return nil
	})

	// 1. Without force: should skip restart because conv-active is not idle
	res, err := mgr.AdoptConversation(t.Context(), AdoptConversationInput{
		ConversationID: "conv-target",
		Project:        "App",
		Title:          "Adopted Safe",
	})
	if err != nil {
		t.Fatalf("AdoptConversation: %v", err)
	}
	if res.DaemonRestarted || restarted {
		t.Errorf("expected restart to be SKIPPED due to active session")
	}
	if res.ActiveSessionsCount != 1 {
		t.Errorf("expected ActiveSessionsCount=1, got %d", res.ActiveSessionsCount)
	}

	// 2. Standalone restart with force_restart=true: should bypass idle check
	resForce, err := mgr.AdoptConversation(t.Context(), AdoptConversationInput{
		ForceRestart: true,
	})
	if err != nil {
		t.Fatalf("standalone force restart: %v", err)
	}
	if !resForce.DaemonRestarted || !restarted {
		t.Errorf("expected force restart to succeed")
	}
}
