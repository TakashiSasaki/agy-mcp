//go:build linux || darwin

package manager

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/proc"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// Mutation guard: the refused fresh request must release its creation claim;
// creating the missing directory makes the identical retry executable once.
func TestWorkspacePreflightRetryAfterMkdir(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}), defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	cwd := filepath.Join(t.TempDir(), "new-workspace")
	req := StartRequest{Prompt: "report", Cwd: cwd, IdempotencyKey: "make-workspace"}
	_, err := m.StartJob(req)
	start, ok := errors.AsType[*StartError](err)
	if !ok || start.Code != "workspace_unavailable" {
		t.Fatalf("expected cwd refusal: %v", err)
	}
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	job, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("same key not reusable after mkdir: %v", err)
	}
	deferJobDone(t, m, job.ID)
	ids, err := m.store.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("expected one new job: %v %v", ids, err)
	}
}

// Mutation guard: dropping the unavailable-alias replay check loses a
// completed job when the originally submitted symlink disappears.
func TestWorkspaceReplayWithUnavailableAlias(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	target := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}), defaultTimeout: time.Minute, maxConcurrency: 4})
	req := StartRequest{Prompt: "report", Cwd: alias, IdempotencyKey: "alias-retry"}
	job, err := m.StartJob(req)
	if err != nil {
		t.Fatal(err)
	}
	deferJobDone(t, m, job.ID)
	testutil.WaitFor(t, 15*time.Second, func() bool { _, ok := m.store.ExitCode(job.ID); return ok }, "fake supervisor completion")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if replay, err := m.StartJob(req); err != nil || replay.ID != job.ID || replay.State != StateDone {
		t.Fatalf("deleted alias lost replay: %+v %v", replay, err)
	}
	canonical := req
	canonical.Cwd = target
	if replay, err := m.StartJob(canonical); err != nil || replay.ID != job.ID {
		t.Fatalf("canonical cwd lost replay: %+v %v", replay, err)
	}
	changed := req
	changed.Prompt = "different request"
	requireWorkspaceReplayConflict(t, m, changed)
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), alias); err != nil {
		t.Fatal(err)
	}
	if replay, err := m.StartJob(req); err != nil || replay.ID != job.ID {
		t.Fatalf("dangling alias lost replay: %+v %v", replay, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	requireWorkspaceReplayConflict(t, m, req)
	ids, err := m.store.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("expected exactly one retained job: %v %v", ids, err)
	}
}

func requireWorkspaceReplayConflict(t *testing.T, m *Manager, req StartRequest) {
	t.Helper()
	job, err := m.StartJob(req)
	start, ok := errors.AsType[*StartError](err)
	if !ok || start.Code != "idempotency_conflict" || job.ID != "" {
		t.Fatalf("changed request should be refused: %+v %v", job, err)
	}
}

// Mutation guards: StartJob must invoke the Git preflight, persist expectations,
// and replay before rechecking a repository changed by a completed job.
func TestGitPreconditionsStartAndReplayAfterHeadChanges(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	createTestGitRepo(t, repo)
	head, err := readGitHead(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}), defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	req := StartRequest{Prompt: "report", Cwd: repo, IdempotencyKey: "guarded", ExpectedGitBranch: "other", ExpectedGitHead: head}
	job, err := m.StartJob(req)
	if job.ID != "" {
		deferJobDone(t, m, job.ID)
	}
	start, ok := errors.AsType[*StartError](err)
	if !ok || start.Code != "workspace_mismatch" || start.JobStarted == nil || *start.JobStarted {
		t.Fatalf("fresh mismatch started or misclassified: %+v %v", job, err)
	}
	ids, err := m.store.List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("mismatch allocated a job: %v %v", ids, err)
	}
	req.ExpectedGitBranch = "main"
	job, err = m.StartJob(req)
	if err != nil {
		t.Fatalf("matching retry: %v", err)
	}
	deferJobDone(t, m, job.ID)
	testutil.WaitFor(t, 15*time.Second, func() bool { _, ok := m.store.ExitCode(job.ID); return ok }, "fake supervisor completion")
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ExpectedGitBranch != "main" || meta.ExpectedGitHead != head {
		t.Fatalf("expectations not persisted: %+v", meta)
	}
	for _, args := range [][]string{{"checkout", "-b", "changed"}, {"commit", "--allow-empty", "-m", "advance HEAD"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	replay, err := m.StartJob(req)
	if err != nil || replay.ID != job.ID {
		t.Fatalf("completed replay rechecked mutable Git state: %+v %v", replay, err)
	}
	req.ExpectedGitHead, err = readGitHead(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.StartJob(req)
	start, ok = errors.AsType[*StartError](err)
	if !ok || start.Code != "idempotency_conflict" {
		t.Fatalf("changed expected HEAD reused bound key: %v", err)
	}
}
