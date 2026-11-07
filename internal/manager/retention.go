package manager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// retentionStart is shared by GC, artifact expiry and frozen terminal elapsed.
// Callers first establish terminal/dead state. Receipt timestamps outrank a
// durable observation estimate; progress/output mtimes are not completion.
func (m *Manager) retentionStart(meta jobstore.Meta, terminal, observe bool) (time.Time, error) {
	dir, err := m.store.Dir(meta.ID)
	if err != nil {
		return time.Time{}, err
	}
	names := []string{jobstore.ResultFile}
	if terminal {
		names = []string{jobstore.ExitCodeFile, jobstore.ResultFile}
	}
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if !info.Mode().IsRegular() {
			return time.Time{}, fmt.Errorf("completion receipt %q is not a regular file", name)
		}
		return info.ModTime(), nil
	}
	return m.store.TerminalObservedAt(meta, observe)
}
