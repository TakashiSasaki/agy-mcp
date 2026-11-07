package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Mutation guard: artifact expiry and GC use the same completion/observation
// boundary, independent of how long the job ran before becoming terminal.
func TestArtifactExpiryMatchesCollection(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		m, store, id, dir := stageEvidenceJob(t, false)
		meta, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		meta.StartedAt = time.Now().Add(-48 * time.Hour)
		if err := store.UpdateMeta(meta); err != nil {
			t.Fatal(err)
		}
		path := jobstore.ExitCodePath(dir)
		if !terminal {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if got := m.artifactExpiry(meta); got != "" {
				t.Fatalf("unobserved interruption invented expiry: %s", got)
			}
			if m.gcEvaluate(m.loadJob(id), time.Now().Add(-m.cfg.JobTTL)) {
				t.Fatal("first interrupted observation removed result")
			}
			path = filepath.Join(dir, jobstore.TerminalObservedFile+".0.0")
		}
		end := time.Now().Add(-30 * time.Minute).UTC()
		if err := os.Chtimes(path, end, end); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		end = info.ModTime()
		page, err := m.ListArtifacts(ListArtifactsRequest{JobID: id})
		want := end.Add(m.cfg.JobTTL).UTC().Format(time.RFC3339Nano)
		if err != nil || page.ExpiresAt != want {
			t.Fatalf("expiry does not match completion/observation, terminal=%v: %+v want %s err %v", terminal, page, want, err)
		}
		if m.gcEvaluate(m.loadJob(id), end.Add(-time.Nanosecond)) {
			t.Fatal("GC deleted before advertised threshold")
		}
		if !m.gcEvaluate(m.loadJob(id), end.Add(time.Nanosecond)) {
			t.Fatal("GC kept job beyond retention threshold")
		}
	}
}

func TestArtifactExpiryRereadsNewlyPublishedExitMarker(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, false)
	end := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
	start := end.Add(-time.Minute)
	if err := jobstore.WriteProcessDir(dir, jobstore.ProcessOutcome{
		Source: jobstore.ProcessOutcomeSource, PID: 1, StartedAt: &start,
		EndedAt: end.Add(-2 * time.Second), ExitCode: new(0),
	}); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, jobstore.ProcessFile)
	if err := os.Chtimes(stamp, end.Add(-time.Second), end.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(jobstore.ExitCodePath(dir), end, end); err != nil {
		t.Fatal(err)
	}
	m.store = &reReadExitStore{jobStore: m.store, id: id}
	page, err := m.ListArtifacts(ListArtifactsRequest{JobID: id})
	want := end.Add(m.cfg.JobTTL).Format(time.RFC3339Nano)
	if err != nil || page.ExpiresAt != want {
		t.Fatalf("expiry = %q, want newly published exit marker expiry %q: %v", page.ExpiresAt, want, err)
	}
}

// Mutation guards: live jobs and disabled expiry must not report a deadline.
func TestArtifactExpiryOmitsLiveAndDisabled(t *testing.T) {
	m, _, id, dir := stageEvidenceJob(t, true)
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("in progress"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := m.ListArtifacts(ListArtifactsRequest{JobID: id})
	if err != nil || page.ExpiresAt != "" {
		t.Fatalf("live output was mistaken for completion: %+v %v", page, err)
	}
	m, _, id, _ = stageEvidenceJob(t, false)
	m.cfg.JobTTL = 0
	page, err = m.ListArtifacts(ListArtifactsRequest{JobID: id})
	if err != nil || page.ExpiresAt != "" {
		t.Fatalf("disabled expiry reported a deadline: %+v %v", page, err)
	}
}
