//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Mutation guard: openArtifactFile rejects special files after a nonblocking
// open, rather than treating a pipe as bounded seekable evidence.
func TestArtifactSpecialFileBoundary(t *testing.T) {
	root := t.TempDir()
	pipe := filepath.Join(root, "output.pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openArtifactFile([]string{root}, pipe)
	if f != nil {
		_ = f.Close()
	}
	if err == nil {
		t.Fatal("named pipe accepted as regular artifact evidence")
	}
	info, err := os.Stat(pipe)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("invalid pipe fixture", err)
	}
}
