package manager

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Mutation guard: artifact expiry must use the same StartedAt + JobTTL boundary
// as garbage collection, including dead jobs with no completion files.
func TestArtifactExpiryMatchesCollection(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		m, store, id, dir := stageEvidenceJob(t, false)
		meta, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		meta.StartedAt = time.Now().Add(-2 * time.Hour).UTC()
		if err := store.UpdateMeta(meta); err != nil {
			t.Fatal(err)
		}
		if !terminal {
			if err := os.Remove(jobstore.ExitCodePath(dir)); err != nil {
				t.Fatal(err)
			}
		}
		page, err := m.ListArtifacts(ListArtifactsRequest{JobID: id})
		want := meta.StartedAt.Add(m.cfg.JobTTL).Format(time.RFC3339Nano)
		if err != nil || page.ExpiresAt != want {
			t.Fatalf("expiry must match creation-based GC, terminal=%v: got %+v, want %s, error %v", terminal, page, want, err)
		}
		removed, err := m.GarbageCollect()
		if err != nil || !slices.Contains(removed, id) {
			t.Fatalf("expired job was not collected: %v %v", removed, err)
		}
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
