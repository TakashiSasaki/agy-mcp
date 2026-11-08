//go:build linux || darwin

package jobstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRetireDetachesBeforeDiskCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission used to defer deletion")
	}
	state := t.TempDir()
	s := New(state)
	const id = "expired"
	dir, err := s.Create(Meta{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "evidence"), []byte("keep until deletion is possible"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(state, retiredJobsDir, id, "blocked")
	t.Cleanup(func() {
		_ = os.Chmod(blocked, 0o700)
		_ = os.Chmod(detached, 0o700)
	})
	if err := s.Retire(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("job is still published: %v", err)
	}
	if ids, err := s.List(); err != nil || len(ids) != 0 {
		t.Fatalf("retired job remains in the scan: %v, %v", ids, err)
	}
	if err := os.Chmod(detached, 0o700); err != nil {
		t.Fatalf("expected a deferred directory: %v", err)
	}
	if err := s.CleanupRetired(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(detached); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deferred directory was not cleaned: %v", err)
	}
}
