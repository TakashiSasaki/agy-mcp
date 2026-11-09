package manager

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
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
		not_fully_idle numeric NOT NULL DEFAULT false,
		raw_summary blob
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	// Mock raw_summary containing Field 15 (title: "") and Field 17.18 (project_id: "default-cli-project")
	f15 := protohelper.MakeLengthDelimited(15, protohelper.MakeLengthDelimited(1, []byte("")))
	f17 := protohelper.MakeLengthDelimited(17, protohelper.MakeLengthDelimited(18, []byte("default-cli-project")))
	mockRaw := append(f15, f17...)

	// Insert orphan session without title or project, with existing workspace URI
	origWorkspace := `["file:///data/existing-ws"]`
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, not_fully_idle, last_modified_time, raw_summary)
		VALUES ('conv-target-1', '', 'How do I optimize this Go codebase for concurrency?', ?, 'default-cli-project', 0, '2026-11-09 10:00:00+00:00', ?)
	`, origWorkspace, mockRaw)
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
	restartTrue := true

	// 1. First adoption with restart_daemon: true
	res1, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "Target App", // resolve by name
		Title:          "Concurrency Optimization",
		RestartDaemon:  &restartTrue,
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
	if !res1.MetadataUpdated {
		t.Errorf("expected metadata_updated=true")
	}
	if !res1.PostRestartVerified {
		t.Errorf("expected post_restart_verified=true")
	}
	if atomic.LoadInt32(&restartCalls) != 1 {
		t.Errorf("expected 1 restart call, got %d", atomic.LoadInt32(&restartCalls))
	}

	// Verify DB state and workspace preservation
	var dbPID, dbTitle, dbURI string
	var dbRaw []byte
	_ = db.QueryRow("SELECT project_id, title, workspace_uris, raw_summary FROM conversation_summaries WHERE conversation_id = 'conv-target-1'").Scan(&dbPID, &dbTitle, &dbURI, &dbRaw)
	if dbPID != "proj-uuid-1234" {
		t.Errorf("DB project_id = %s, want proj-uuid-1234", dbPID)
	}
	if dbTitle != "Concurrency Optimization" {
		t.Errorf("DB title = %s, want 'Concurrency Optimization'", dbTitle)
	}
	// Check non-destructive workspace merge: both existing and new target root should be present
	if dbURI != `["file:///data/existing-ws","file:///data/target-app"]` {
		t.Errorf("DB workspace_uris = %s, want merged list", dbURI)
	}
	// Check raw_summary updated in DB
	f17Bytes, ok := protohelper.GetFieldBytes(dbRaw, 17)
	if !ok {
		t.Fatalf("raw_summary missing field 17")
	}
	subPID, ok := protohelper.GetFieldBytes(f17Bytes, 18)
	if !ok || string(subPID) != "proj-uuid-1234" {
		t.Errorf("raw_summary field 17.18 = %q, want 'proj-uuid-1234'", string(subPID))
	}

	// 2. Second adoption with identical parameters (Idempotent No-op)
	res2, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "proj-uuid-1234", // resolve by ID
		Title:          "Concurrency Optimization",
		RestartDaemon:  &restartTrue,
	})
	if err != nil {
		t.Fatalf("second AdoptConversation: %v", err)
	}
	if !res2.Success {
		t.Errorf("expected success=true")
	}
	if !res2.AlreadyConverted {
		t.Errorf("expected already_converted=true on repeated adoption")
	}
	if res2.MetadataUpdated {
		t.Errorf("expected metadata_updated=false on idempotent run")
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

	previewText := "Please explain how Antigravity manages local worktrees and bare git repositories across diverse agent branches."
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, not_fully_idle, last_modified_time)
		VALUES ('conv-preview-derive', '', ?, '', 'default-cli-project', 0, '2026-11-09 10:00:00+00:00')
	`, previewText)
	if err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{
		"id": "proj-uuid-5678",
		"name": "Docs Project",
		"projectResources": {"resources": [{"folderUri": "file:///data/docs"}]},
		"isWorkspaceOnly": false
	}`
	if err := os.WriteFile(filepath.Join(projDir, "docs.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	ctx := t.Context()

	// Title is omitted: should derive 60-char truncated title with "..."
	res, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-preview-derive",
		Project:        "Docs Project",
	})
	if err != nil {
		t.Fatalf("AdoptConversation: %v", err)
	}

	expectedPrefix := "Please explain how Antigravity manages local worktrees and b..."
	if res.Title != expectedPrefix {
		t.Errorf("derived Title = %q, want %q", res.Title, expectedPrefix)
	}
}

func TestRestartSyncDaemonIdleCheck(t *testing.T) {
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

	// Insert an ACTIVE running conversation (not_fully_idle = 1)
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, status, not_fully_idle, last_modified_time)
		VALUES ('active-conv-1', 'Active Session', 'running prompt', '', 'default-cli-project', 'CASCADE_RUN_STATUS_RUNNING', 1, '2026-11-09 10:00:00+00:00')
	`)
	if err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		projectsDir:         filepath.Join(dir, "projects"),
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	var restartCalls int32
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error {
		atomic.AddInt32(&restartCalls, 1)
		return nil
	})

	ctx := t.Context()

	// 1. Standalone graceful restart: should detect active session and SKIP restart
	restartRes, err := mgr.RestartSyncDaemon(ctx, false)
	if err != nil {
		t.Fatalf("RestartSyncDaemon: %v", err)
	}
	if restartRes.DaemonRestarted {
		t.Errorf("expected daemon_restarted=false when active tasks exist")
	}
	if restartRes.ActiveSessionsCount != 1 {
		t.Errorf("expected ActiveSessionsCount=1, got %d", restartRes.ActiveSessionsCount)
	}
	if atomic.LoadInt32(&restartCalls) != 0 {
		t.Errorf("expected 0 restart calls, got %d", atomic.LoadInt32(&restartCalls))
	}

	// 2. Standalone force restart: should bypass idle check
	forceRes, err := mgr.RestartSyncDaemon(ctx, true)
	if err != nil {
		t.Fatalf("standalone force restart: %v", err)
	}
	if !forceRes.DaemonRestarted {
		t.Errorf("expected daemon_restarted=true with force=true")
	}
	if atomic.LoadInt32(&restartCalls) != 1 {
		t.Errorf("expected 1 restart call after force restart, got %d", atomic.LoadInt32(&restartCalls))
	}
}

func TestAdoptConversationRequiresConversationID(t *testing.T) {
	mgr := &Manager{}
	_, err := mgr.AdoptConversation(context.Background(), AdoptConversationInput{
		ConversationID: "",
		Project:        "any-proj",
	})
	if err == nil || err.Error() != "conversation_id is required" {
		t.Errorf("expected 'conversation_id is required', got %v", err)
	}
}
