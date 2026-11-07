package manager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// Mutation guard: removing the fresh-work os.Stat/IsDir preflight changes this
// refusal or reaches conversation/CLI handling, rather than reporting the cwd.
func TestFreshWorkspacePreflight(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	base := t.TempDir()
	file := filepath.Join(base, "regular-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(base, "missing"), file} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			m := newManager(t, managerOpts{withCacheFile: true})
			// Cwd diagnosis must precede mutable cache and CLI probes as well.
			if err := os.WriteFile(m.cacheFile, []byte(`{"torn`), 0o600); err != nil {
				t.Fatal(err)
			}
			req := StartRequest{Prompt: "review", Cwd: path, IdempotencyKey: "fresh", ContinueLatest: true}
			for range 2 {
				job, err := m.StartJob(req)
				start, ok := errors.AsType[*StartError](err)
				if !ok || start.Code != "workspace_unavailable" || start.NextAction != "change_request" || start.JobStarted == nil || *start.JobStarted || start.Retryable || job.ID != "" {
					t.Fatalf("wrong cwd refusal: %+v %v", job, err)
				}
			}
			ids, err := m.store.List()
			if err != nil || len(ids) != 0 {
				t.Fatalf("refusal created jobs: %v %v", ids, err)
			}
		})
	}
}

// Mutation guard: placing the preflight before replay loses a completed job
// after its former directory is removed.
func TestWorkspacePreflightPreservesRemovedDirectoryReplay(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	m := newManager(t, managerOpts{defaultTimeout: time.Minute, withCacheFile: true})
	cwd := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	req, err := m.normalizeRequest(StartRequest{Prompt: "report", Cwd: cwd, IdempotencyKey: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	meta := jobstore.Meta{ID: "retained", PID: -1, StartedAt: time.Now(), Cwd: req.Cwd, Args: buildAgyArgs(req), IdempotencyKey: req.IdempotencyKey, RequestKey: requestKey(req)}
	if _, err := m.store.Create(meta); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	job, err := m.StartJob(req)
	if err != nil || job.ID != meta.ID || job.State != "done" {
		t.Fatalf("removed cwd blocked completed replay: %+v %v", job, err)
	}
	ids, err := m.store.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("replay duplicated work: %v %v", ids, err)
	}
}
