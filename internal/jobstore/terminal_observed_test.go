package jobstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTerminalObservationIsExclusiveAndStable(t *testing.T) {
	state := t.TempDir()
	s := New(state)
	dir, err := s.Create(Meta{ID: "job"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TerminalObservedAt(Meta{ID: "job"}, false); !os.IsNotExist(err) {
		t.Fatalf("read created or invented observation: %v", err)
	}
	if _, err := s.TerminalObservedAt(Meta{ID: "job"}, true); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, TerminalObservedFile+".0.0"), fixed, fixed); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := New(state).TerminalObservedAt(Meta{ID: "job"}, true)
			if err != nil || !got.Equal(fixed) {
				t.Errorf("observation reset across concurrent reopen: %v %v", got, err)
			}
		})
	}
	wg.Wait()
}

func TestTerminalObservationRejectsSymlink(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.Create(Meta{ID: "job"})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, TerminalObservedFile+".0.0")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, create := range []bool{false, true} {
		if _, err := s.TerminalObservedAt(Meta{ID: "job"}, create); err == nil {
			t.Fatalf("symlink marker accepted, create=%v", create)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
}
