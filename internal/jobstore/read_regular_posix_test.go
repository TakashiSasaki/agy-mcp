//go:build linux || darwin

package jobstore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Mutation guard: replacing the nonblocking regular-file check with os.ReadFile
// waits for a writer and never returns the damaged-entry error.
func TestReadReplacedRejectsUnpairedFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), ExitCodeFile)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readReplaced(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("FIFO did not return a regular-file error: %v", err)
		}
	case <-time.After(time.Second):
		// Release the reader even when run against the faulty implementation.
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(fd)
		}
		t.Fatal("job state read blocked on a FIFO without a writer")
	}
}
