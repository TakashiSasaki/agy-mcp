package manager

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

const workerTestProject = "cloud-project-1"

func testCloudWorker(t *testing.T) (*Manager, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	m := New(config.Config{StateDir: root})
	m.conversationsDBPath = filepath.Join(root, "conversation_summaries.db")
	m.projectsDir = filepath.Join(root, "projects")
	for _, dir := range []string{m.cloudDir(), m.projectsDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	projectConfig := []byte(`{"id":"cloud-project-1","name":"Worker test","projectResources":{"resources":[{"folderUri":"file:///srv/test"}]},"isWorkspaceOnly":false}`)
	if err := os.WriteFile(filepath.Join(m.projectsDir, "test.json"), projectConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", m.conversationsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY, title TEXT, preview TEXT, step_count INTEGER, last_modified_time TEXT,
		status TEXT, not_fully_idle INTEGER, project_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	return m, db
}

func testCloudTerminalJob(t *testing.T, m *Manager, id, conversation, title string) string {
	t.Helper()
	dir, err := m.store.Create(jobstore.Meta{ID: id, StartedAt: time.Now().Add(-time.Hour),
		Cloud: &jobstore.CloudIntent{ProjectID: workerTestProject, Title: title}})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobstore.WriteProgressDir(dir, jobstore.Progress{ConversationID: conversation}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCloudWorkerBatchesAndPreservesRevision(t *testing.T) {
	m, db := testCloudWorker(t)
	now := time.Now().UTC()
	m.now = func() time.Time { return now }
	rev := conversationRevision{modified: now.Add(-time.Minute), steps: 2}
	const first, second = "cloud-first", "cloud-second"
	for _, id := range []string{first, second} {
		trajectory := testAdoptionTrajectory(t, m.cfg.StateDir, id)
		if _, err := trajectory.Exec("CREATE TABLE steps (idx INTEGER PRIMARY KEY); INSERT INTO steps VALUES (0), (1)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO conversation_summaries VALUES (?, '', ?, 2, ?, 'IDLE', 0, 'default-cli-project')",
			id, "Preview "+id, rev.modified.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	dirA := testCloudTerminalJob(t, m, "job-first", first, "Assigned title")
	dirB := testCloudTerminalJob(t, m, "job-second", second, "")
	neighbor := testDaemonConversation("other", "other-project", "Unrelated", rev)
	snapshot := bytes.Clone(neighbor)
	for _, id := range []string{first, second} {
		snapshot = append(snapshot, testDaemonConversation(id, "default-cli-project", "old", conversationRevision{
			modified: rev.modified.Add(-time.Minute), steps: 1})...)
	}
	for _, name := range []string{cliSummariesFile, daemonSummariesFile} {
		if err := os.WriteFile(filepath.Join(m.cfg.StateDir, name), snapshot, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restarts := 0
	m.restartDaemonCmd = func(ctx context.Context, prepare func(context.Context) error) (bool, error) {
		restarts++
		journal, err := m.cloudJournal()
		if err != nil || !journal.NeedsStart {
			t.Fatalf("daemon stopped without durable recovery record: %+v %v", journal, err)
		}
		if err := prepare(ctx); err != nil {
			return false, err
		}
		for _, name := range []string{cliSummariesFile, daemonSummariesFile} {
			b, err := os.ReadFile(filepath.Join(m.cfg.StateDir, name))
			if err != nil || !bytes.Equal(b, neighbor) {
				t.Fatalf("%s should retain only the unrelated entry: %x %v", name, b, err)
			}
		}
		fresh := append(bytes.Clone(neighbor), testDaemonConversation(first, workerTestProject, "Assigned title", rev)...)
		fresh = append(fresh, testDaemonConversation(second, workerTestProject, "Preview "+second, rev)...)
		return true, os.WriteFile(filepath.Join(m.cfg.StateDir, daemonSummariesFile), fresh, 0o600)
	}
	for _, id := range []string{first, second} {
		pending, err := m.cloudConversationPending(id)
		if err != nil || !pending {
			t.Fatalf("continuation should see pending job for %s: %v %v", id, pending, err)
		}
	}
	if drain, err := m.cloudRound(t.Context()); err != nil || drain {
		t.Fatalf("batch: drain=%v err=%v", drain, err)
	}
	for _, dir := range []string{dirA, dirB} {
		receipt, err := jobstore.ReadCloudSync(dir)
		if err != nil || receipt.State != jobstore.CloudConfirmed || receipt.Revision.Steps != 2 {
			t.Fatalf("job receipt: %+v %v", receipt, err)
		}
	}
	now = now.Add(time.Minute)
	replayDir := testCloudTerminalJob(t, m, "job-replay", first, "Assigned title")
	if _, err := m.cloudRound(t.Context()); err != nil {
		t.Fatal(err)
	}
	if receipt, err := jobstore.ReadCloudSync(replayDir); err != nil || receipt.State != jobstore.CloudConfirmed || restarts != 1 {
		t.Fatalf("confirmed revision should skip restart: %+v, restarts=%d, err=%v", receipt, restarts, err)
	}
}

func TestCloudWorkerRecoversIncompleteStopBeforeProcessing(t *testing.T) {
	m, _ := testCloudWorker(t)
	if err := m.writeCloudJSON(cloudJournalFile, cloudMaintenance{NeedsStart: true}); err != nil {
		t.Fatal(err)
	}
	starts := 0
	m.cloudStartDaemon = func(context.Context) error {
		starts++
		if starts == 1 {
			return errors.New("test service is temporarily unavailable")
		}
		return nil
	}
	if _, err := m.cloudRound(t.Context()); err == nil {
		t.Fatal("expected first start to fail")
	}
	journal, err := m.cloudJournal()
	if err != nil || !journal.NeedsStart {
		t.Fatalf("lost recovery marker: %+v %v", journal, err)
	}
	if _, err := m.cloudRound(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal, err = m.cloudJournal()
	if err != nil || journal.NeedsStart || journal.LastRestart.IsZero() || starts != 2 {
		t.Fatalf("did not recover: %+v starts=%d err=%v", journal, starts, err)
	}
}

func TestCloudWaitAndRetentionUseSeparateReceipt(t *testing.T) {
	m, _ := testCloudWorker(t)
	dir := testCloudTerminalJob(t, m, "job-wait", "", "")
	deadline := time.Now().Add(-time.Second)
	st, settled, err := m.WaitCloudJob(t.Context(), "job-wait", deadline, nil)
	if err != nil || settled || st.State == StateRunning || st.CloudSyncState != jobstore.CloudPending {
		t.Fatalf("execution should finish before sync: %+v settled=%v err=%v", st, settled, err)
	}
	if m.gcEvaluate(m.loadJob("job-wait"), time.Now().Add(time.Hour)) {
		t.Fatal("GC removed a pending sync")
	}
	if _, err := m.cloudRound(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, settled, err = m.WaitCloudJob(t.Context(), "job-wait", deadline, nil)
	if err != nil || !settled || st.CloudSyncState != jobstore.CloudFailed || st.CloudSyncReason != "conversation_missing" {
		t.Fatalf("expected explicit missing conversation: %+v settled=%v err=%v", st, settled, err)
	}
	if _, err := os.Stat(filepath.Join(dir, jobstore.CloudSyncFile)); err != nil {
		t.Fatal(err)
	}
}

func TestCloudAdmissionRequiresSupportedWorker(t *testing.T) {
	m, _ := testCloudWorker(t)
	gate, intent, err := m.cloudAdmission(StartRequest{Project: workerTestProject}, "agy")
	refusal, isRefusal := errors.AsType[*StartError](err)
	want := "cloud_sync_unsupported"
	if runtime.GOOS == cloudLinux {
		want = "cloud_sync_unavailable"
	}
	if gate != nil || intent != nil || !isRefusal ||
		refusal.Code != want || refusal.JobStarted == nil || *refusal.JobStarted {
		t.Fatalf("expected pre-admission refusal %s: gate=%v intent=%+v err=%v", want, gate, intent, err)
	}
	if runtime.GOOS == cloudLinux {
		lock, err := m.tryCloudLock(cloudGateName)
		if err != nil || lock == nil {
			t.Fatalf("failed preflight retained the admission lock: %v", err)
		}
		_ = lock.Close()
	}
}

func TestCloudProjectAndTitleBindIdempotency(t *testing.T) {
	req := StartRequest{Prompt: "do work", Project: workerTestProject, Title: "First"}
	meta := jobstore.Meta{RequestKey: requestKey(req)}
	if !sameRequest(meta, req, nil) {
		t.Fatal("same request did not replay")
	}
	for _, changed := range []StartRequest{
		{Prompt: req.Prompt, Project: "another-project", Title: req.Title},
		{Prompt: req.Prompt, Project: req.Project, Title: "Different title"},
	} {
		if sameRequest(meta, changed, nil) {
			t.Fatalf("changed cloud request matched original key: %+v", changed)
		}
	}
}
