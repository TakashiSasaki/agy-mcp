package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// A 25-hour job must still have a full 24-hour retrieval window after finishing.
// This deterministic cutoff test requires no wall-clock wait.
func TestLongJobRetainsFullTTLAfterCompletion(t *testing.T) {
	m := newManager(t, managerOpts{jobTTL: 24 * time.Hour})
	end := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
	meta := jobstore.Meta{ID: "long-finished", StartedAt: end.Add(-25 * time.Hour)}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, jobstore.ExitCodeFile), end, end); err != nil {
		t.Fatal(err)
	}
	if m.gcEvaluate(m.loadJob(meta.ID), end.Add(-24*time.Hour)) {
		t.Fatal("long job deleted immediately after completion")
	}
	if got := m.artifactExpiry(meta); got != end.Add(24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("expiry %q does not provide full completion TTL", got)
	}
	if m.gcEvaluate(m.loadJob(meta.ID), end.Add(-time.Nanosecond)) {
		t.Fatal("job deleted before full retention window")
	}
	if !m.gcEvaluate(m.loadJob(meta.ID), end.Add(time.Nanosecond)) {
		t.Fatal("completed job not collected after full retention window")
	}
}

// Missing terminal receipts use a durable first observation, not old empty
// output or newer progress timestamps. Reads/restarts cannot reset its TTL.
func TestInterruptedRetentionAndElapsedObservationAreDurable(t *testing.T) {
	m := newManager(t, managerOpts{jobTTL: 24 * time.Hour})
	start := time.Now().Add(-25 * time.Hour)
	meta := jobstore.Meta{ID: "interrupted", StartedAt: start}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, jobstore.OutFile)
	if err := os.WriteFile(out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(out, start, start); err != nil {
		t.Fatal(err)
	}
	progress := filepath.Join(dir, jobstore.ProgressFile)
	if err := os.WriteFile(progress, []byte(`{"step_index":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(progress, start.Add(20*time.Hour), start.Add(20*time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, err := m.Status(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Elapsed < 24*time.Hour || first.Elapsed > 26*time.Hour {
		t.Fatalf("empty out produced misleading elapsed: %v", first.Elapsed)
	}
	observed, err := m.store.TerminalObservedAt(meta, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.gcEvaluate(m.loadJob(meta.ID), observed.Add(-24*time.Hour)) {
		t.Fatal("recent observed interruption was removed")
	}
	// New manager over the same store must preserve the first observation.
	again := newManager(t, managerOpts{stateDir: m.cfg.StateDir, jobTTL: 24 * time.Hour})
	second, err := again.Status(meta.ID)
	if err != nil || second.Elapsed != first.Elapsed {
		t.Fatalf("restart changed frozen elapsed: %v %v", second.Elapsed, err)
	}
	marker, err := again.store.TerminalObservedAt(meta, true)
	if err != nil || !marker.Equal(observed) {
		t.Fatalf("restart reset retention marker: %v %v", marker, err)
	}
	want := observed.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if got := again.artifactExpiry(meta); got != want {
		t.Fatalf("interruption expiry %q want %q", got, want)
	}
	if again.gcEvaluate(again.loadJob(meta.ID), observed.Add(-time.Nanosecond)) {
		t.Fatal("interruption removed before full observation TTL")
	}
	if !again.gcEvaluate(again.loadJob(meta.ID), observed.Add(time.Nanosecond)) {
		t.Fatal("interruption retained after full observation TTL")
	}
}

func TestRecoveredResultRetentionUsesReceiptWithoutObservation(t *testing.T) {
	m := newManager(t, managerOpts{jobTTL: time.Hour})
	end := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
	meta := jobstore.Meta{ID: "recovered", StartedAt: end.Add(-25 * time.Hour)}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, jobstore.ResultFile)
	if err := os.WriteFile(result, []byte(`{"status":"SUCCESS","response":"done"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(result, end, end); err != nil {
		t.Fatal(err)
	}
	if m.gcEvaluate(m.loadJob(meta.ID), end.Add(-time.Hour)) {
		t.Fatal("fresh recovered receipt removed")
	}
	if _, err := os.Lstat(filepath.Join(dir, jobstore.TerminalObservedFile+".0.0")); !os.IsNotExist(err) {
		t.Fatalf("receipt should not need observation marker: %v", err)
	}
	if !m.gcEvaluate(m.loadJob(meta.ID), end.Add(time.Nanosecond)) {
		t.Fatal("old recovered receipt retained forever")
	}
}

func TestRecoveredProcessReceiptFreezesElapsedAndRetention(t *testing.T) {
	for _, result := range []string{"saved", "missing", "blocked"} {
		t.Run(result, func(t *testing.T) {
			m := newManager(t, managerOpts{jobTTL: time.Hour})
			end := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
			meta := jobstore.Meta{ID: "recovered-process", StartedAt: end.Add(-3 * time.Hour)}
			dir, err := m.store.Create(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(jobstore.ExitCodePath(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			receipt := jobstore.ProcessOutcome{
				Source: jobstore.ProcessOutcomeSource, PID: 1, StartedAt: &meta.StartedAt,
				EndedAt: end.Add(-time.Second), ExitCode: new(42),
			}
			if err := jobstore.WriteProcessDir(dir, receipt); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(dir, jobstore.ProcessFile), end, end); err != nil {
				t.Fatal(err)
			}
			switch result {
			case "saved":
				if err := jobstore.WriteResultDir(dir, []byte(`{"status":"SUCCESS","response":"saved answer"}`)); err != nil {
					t.Fatal(err)
				}
				end = end.Add(time.Second)
				if err := os.Chtimes(jobstore.ResultPath(dir), end, end); err != nil {
					t.Fatal(err)
				}
			case "blocked":
				if err := os.Mkdir(jobstore.ResultPath(dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(jobstore.OutPath(dir), []byte("saved answer"), 0o600); err != nil {
				t.Fatal(err)
			}
			if expiry := m.artifactExpiry(meta); expiry != end.Add(time.Hour).Format(time.RFC3339Nano) {
				t.Fatalf("artifact expiry = %q, want one hour after %v", expiry, end)
			}
			st, err := m.Status(meta.ID)
			if err != nil || st.State != StateFailed || st.Result != "saved answer" || st.Partial != (result != "saved") || st.Elapsed != end.Sub(meta.StartedAt) {
				t.Fatalf("unexpected recovered status: %+v, %v", st, err)
			}
			if m.gcEvaluate(m.loadJob(meta.ID), end.Add(-time.Nanosecond)) {
				t.Fatal("GC removed recovered job before its advertised expiry")
			}
			if !m.gcEvaluate(m.loadJob(meta.ID), end.Add(time.Nanosecond)) {
				t.Fatal("GC retained recovered job beyond its advertised expiry")
			}
		})
	}
}

// Status/GC can observe a PID-zero entry between Create and PID publication.
// Even an observer holding that stale snapshot after publication must not set
// the eventual interrupted supervisor's retention window or elapsed time.
func TestStartupObservationDoesNotPoisonPublishedSupervisor(t *testing.T) {
	m := newManager(t, managerOpts{jobTTL: 24 * time.Hour})
	start := time.Now().Add(-25 * time.Hour)
	pending := jobstore.Meta{ID: "publishing", StartedAt: start}
	dir, err := m.store.Create(pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(pending.ID); err != nil {
		t.Fatal(err)
	}
	pendingMarker := filepath.Join(dir, jobstore.TerminalObservedFile+".0.0")
	if err := os.Chtimes(pendingMarker, start, start); err != nil {
		t.Fatal(err)
	}
	// This PID is deliberately nonexistent: publication followed by a crash
	// without a receipt is the final state under test.
	published := pending
	published.PID = 2147483647
	published.StartTimeTicks = 12345
	if err := m.store.UpdateMeta(published); err != nil {
		t.Fatal(err)
	}
	// Recreate the marker as a delayed observer with a prepublication snapshot.
	// This makes a remove-after-publication workaround insufficient.
	if err := os.Remove(pendingMarker); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.TerminalObservedAt(pending, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pendingMarker, start, start); err != nil {
		t.Fatal(err)
	}
	if m.gcEvaluate(m.loadJob(published.ID), time.Now().Add(-24*time.Hour)) {
		t.Fatal("startup observation deleted recently interrupted published supervisor")
	}
	st, err := m.Status(published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Elapsed < 24*time.Hour {
		t.Fatalf("startup observation shortened published supervisor elapsed: %v", st.Elapsed)
	}
	observed, err := m.store.TerminalObservedAt(published, false)
	if err != nil || observed.Before(start.Add(24*time.Hour)) {
		t.Fatalf("published generation reused startup timestamp: %v %v", observed, err)
	}
	if got := m.artifactExpiry(published); got != observed.Add(24*time.Hour).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("expiry %q does not use published generation", got)
	}
}
