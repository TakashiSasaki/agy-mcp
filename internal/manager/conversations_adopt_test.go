package manager

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
	_ "modernc.org/sqlite"
)

func testAdoptionTrajectory(t *testing.T, dir, id string) *sql.DB {
	t.Helper()
	trajectoryDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(trajectoryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(trajectoryDir, id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE trajectory_metadata_blob (id text PRIMARY KEY, data blob)"); err != nil {
		t.Fatal(err)
	}
	data := protohelper.MakeLengthDelimited(trajectoryProjectField, []byte("default-cli-project"))
	if _, err := db.Exec("INSERT INTO trajectory_metadata_blob VALUES ('main', ?)", data); err != nil {
		t.Fatal(err)
	}
	return db
}

func assertTrajectoryProject(t *testing.T, db *sql.DB, expected string) {
	t.Helper()
	var data []byte
	if err := db.QueryRow("SELECT data FROM trajectory_metadata_blob WHERE id = 'main'").Scan(&data); err != nil {
		t.Fatal(err)
	}
	project, _ := protohelper.GetFieldBytes(data, trajectoryProjectField)
	if string(project) != expected {
		t.Errorf("trajectory project = %q, want %q", project, expected)
	}
}

func TestAdoptConversationSuccessAndIdempotency(t *testing.T) {
	const title = "Concurrency $2 \"Optimization\"\n日本語"
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
	mockRaw := protohelper.MakeLengthDelimited(15, protohelper.MakeLengthDelimited(1, nil))
	mockRaw = append(mockRaw, protohelper.MakeLengthDelimited(17, protohelper.MakeLengthDelimited(18, []byte("default-cli-project")))...)

	// Insert orphan session without title or project, with existing workspace URI
	origWorkspace := `["file:///data/existing-ws"]`
	_, err = db.Exec(`
		INSERT INTO conversation_summaries (conversation_id, title, preview, workspace_uris, project_id, not_fully_idle, last_modified_time, raw_summary)
		VALUES ('conv-target-1', '', 'How do I optimize this Go codebase for concurrency?', ?, 'default-cli-project', 0, '2026-11-09 10:00:00+00:00', ?)
	`, origWorkspace, mockRaw)
	if err != nil {
		t.Fatal(err)
	}
	trajectory := testAdoptionTrajectory(t, dir, "conv-target-1")
	annotDir := filepath.Join(dir, "annotations")
	if err := os.Mkdir(annotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	annotationPath := filepath.Join(annotDir, "conv-target-1.pbtxt")
	const otherFields = "tags:\"literal title: \\\"keep me\\\"\"\narchived:true\n# title:\"ignore\"\n"
	const originalAnnotation = otherFields + "title:\"old\"\n"
	if err := os.WriteFile(annotationPath, []byte(originalAnnotation), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, daemonSummariesFile)
	cliCachePath := filepath.Join(dir, cliSummariesFile)
	revision := conversationRevision{modified: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
	neighbor := testDaemonConversation("other-conversation", "different-project", "Keep me", conversationRevision{})
	neighbor = append(neighbor, protohelper.MakeLengthDelimited(4, []byte("unknown top-level field"))...)
	snapshot := append(bytes.Clone(neighbor), testDaemonConversation("conv-target-1", "default-cli-project", "Old title", revision)...)
	if err := os.WriteFile(cachePath, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliCachePath, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}

	// Create registered project
	projDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectResources(t, projDir, "target.json", "proj-uuid-1234", "Target App", projectFileURI(dir, ""))

	mgr := &Manager{
		projectsDir:         projDir,
		dataDir:             t.TempDir(),
		conversationsDBPath: dbPath,
	}

	var restartCalls atomic.Int32
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error {
		restartCalls.Add(1)
		cache, err := os.ReadFile(cachePath)
		if err != nil {
			return err
		}
		if !bytes.Equal(cache, neighbor) {
			t.Errorf("daemon cache should retain only untouched entries during refresh")
		}
		if cliCache, err := os.ReadFile(cliCachePath); err != nil || !bytes.Equal(cliCache, neighbor) {
			t.Errorf("CLI cache should retain only untouched entries during refresh: %v", err)
		}
		cache = append(cache, testDaemonConversation("conv-target-1", "proj-uuid-1234", title, revision)...)
		return os.WriteFile(cachePath, cache, 0o600)
	})

	ctx := t.Context()
	restartTrue := true

	request := AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "Target App", // resolve by name
		Title:          title,
		RestartDaemon:  &restartTrue,
	}
	if _, err := db.Exec("CREATE TRIGGER reject_adoption BEFORE UPDATE ON conversation_summaries BEGIN SELECT RAISE(ABORT, 'injected summary failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AdoptConversation(ctx, request); err == nil || !strings.Contains(err.Error(), "injected summary failure") {
		t.Fatalf("expected summary failure, got %v", err)
	}
	assertTrajectoryProject(t, trajectory, "default-cli-project")
	if got, err := os.ReadFile(annotationPath); err != nil || string(got) != originalAnnotation {
		t.Fatalf("failed adoption changed the annotation: %q, %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_adoption"); err != nil {
		t.Fatal(err)
	}

	res1, err := mgr.AdoptConversation(ctx, request)
	if err != nil {
		t.Fatalf("first AdoptConversation: %v", err)
	}
	assertInitialAdoption(t, res1, title)
	if restartCalls.Load() != 1 {
		t.Errorf("expected 1 restart call, got %d", restartCalls.Load())
	}

	assertAdoptionPersistence(t, db, trajectory, title, origWorkspace, annotationPath, otherFields+"title:"+strconv.Quote(title)+"\n")

	// 2. Second adoption with identical parameters (Idempotent No-op)
	res2, err := mgr.AdoptConversation(ctx, AdoptConversationInput{
		ConversationID: "conv-target-1",
		Project:        "proj-uuid-1234", // resolve by ID
		Title:          title,
		RestartDaemon:  &restartTrue,
	})
	if err != nil {
		t.Fatalf("second AdoptConversation: %v", err)
	}
	assertAdoptionReplay(t, res2, restartCalls.Load())

	// A previous interrupted or legacy adoption can leave the SQL columns
	// correct while the authoritative trajectory still has the prior project.
	old := protohelper.MakeLengthDelimited(trajectoryProjectField, []byte("default-cli-project"))
	if _, err := trajectory.Exec("UPDATE trajectory_metadata_blob SET data = ? WHERE id = 'main'", old); err != nil {
		t.Fatal(err)
	}
	repaired, err := mgr.AdoptConversation(ctx, request)
	if err != nil || repaired.AlreadyConverted || !repaired.TrajectoryBlobUpdated || repaired.MetadataUpdated {
		t.Fatalf("expected only the stale trajectory to be repaired: %+v, %v", repaired, err)
	}
}

func assertInitialAdoption(t *testing.T, res1 *AdoptConversationResult, title string) {
	t.Helper()
	if !res1.Success || res1.AlreadyConverted || !res1.MetadataUpdated {
		t.Errorf("expected local metadata to be updated on first adoption, got %+v", res1)
	}
	if res1.ProjectID != "proj-uuid-1234" {
		t.Errorf("expected ProjectID=proj-uuid-1234, got %s", res1.ProjectID)
	}
	if res1.Title != title {
		t.Errorf("title = %q, want %q", res1.Title, title)
	}
	if !res1.DaemonRestarted || !res1.PostRestartVerified || res1.CloudSyncState != ConversationSyncConfirmed {
		t.Errorf("expected the restarted daemon to confirm adoption, got %+v", res1)
	}
	if !res1.DaemonCacheInvalidated || !res1.CLICacheInvalidated {
		t.Errorf("expected stale entries to be invalidated in both snapshots: %+v", res1)
	}
}

func assertAdoptionPersistence(t *testing.T, db, trajectory *sql.DB, title, origWorkspace, annotationPath, expectedAnnotation string) {
	t.Helper()
	// Verify DB state and workspace preservation
	var dbPID, dbTitle, dbURI string
	var dbRaw []byte
	if err := db.QueryRow("SELECT project_id, title, workspace_uris, raw_summary FROM conversation_summaries WHERE conversation_id = 'conv-target-1'").Scan(&dbPID, &dbTitle, &dbURI, &dbRaw); err != nil {
		t.Fatal(err)
	}
	if dbPID != "proj-uuid-1234" {
		t.Errorf("DB project_id = %s, want proj-uuid-1234", dbPID)
	}
	if dbTitle != title {
		t.Errorf("DB title = %q, want %q", dbTitle, title)
	}
	if dbURI != origWorkspace {
		t.Errorf("DB workspace_uris = %s, want original workspace", dbURI)
	}
	assertTrajectoryProject(t, trajectory, "proj-uuid-1234")
	if got, err := os.ReadFile(annotationPath); err != nil || string(got) != expectedAnnotation {
		t.Fatalf("annotation did not preserve the other fields and quote the new title: %q, %v", got, err)
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

}

func assertAdoptionReplay(t *testing.T, res2 *AdoptConversationResult, restartCount int32) {
	t.Helper()
	if !res2.Success || !res2.AlreadyConverted ||
		res2.MetadataUpdated || res2.TrajectoryBlobUpdated || res2.AnnotationsUpdated ||
		res2.DaemonRestarted || res2.PostRestartVerified || res2.DaemonCacheInvalidated || res2.CLICacheInvalidated {
		t.Errorf("expected no writes or restart on an idempotent run, got %+v", res2)
	}
	if restartCount != 1 {
		t.Errorf("repeat adoption restarted the daemon")
	}

}

func TestAdoptConversationTitleDerivationFromPreview(t *testing.T) {
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
	testAdoptionTrajectory(t, dir, "conv-preview-derive")

	projDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectResources(t, projDir, "docs.json", "proj-uuid-5678", "Docs Project", projectFileURI(dir, ""))

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
	defer func() { _ = db.Close() }()

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

	var restartCalls atomic.Int32
	mgr.SetRestartDaemonCmdForTest(func(ctx context.Context) error {
		restartCalls.Add(1)
		return nil
	})

	ctx := t.Context()

	if _, err := mgr.AdoptConversation(ctx, AdoptConversationInput{ConversationID: "active-conv-1"}); err == nil || !strings.Contains(err.Error(), "conversation is busy") {
		t.Errorf("expected active conversation adoption to be refused, got %v", err)
	}

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
	if calls := restartCalls.Load(); calls != 0 {
		t.Errorf("expected 0 restart calls, got %d", calls)
	}

	// 2. Standalone force restart: should bypass idle check
	forceRes, err := mgr.RestartSyncDaemon(ctx, true)
	if err != nil {
		t.Fatalf("standalone force restart: %v", err)
	}
	if !forceRes.DaemonRestarted {
		t.Errorf("expected daemon_restarted=true with force=true")
	}
	if calls := restartCalls.Load(); calls != 1 {
		t.Errorf("expected 1 restart call after force restart, got %d", calls)
	}
}

func TestAdoptConversationRequiresConversationID(t *testing.T) {
	mgr := &Manager{}
	_, err := mgr.AdoptConversation(t.Context(), AdoptConversationInput{
		ConversationID: "",
		Project:        "any-proj",
	})
	if err == nil || err.Error() != "conversation_id is required" {
		t.Errorf("expected 'conversation_id is required', got %v", err)
	}
}
