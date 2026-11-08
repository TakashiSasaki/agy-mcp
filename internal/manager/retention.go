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
func (m *Manager) retentionStart(meta jobstore.Meta, hasExitMarker, observe bool) (time.Time, error) {
	dir, err := m.store.Dir(meta.ID)
	if err != nil {
		return time.Time{}, err
	}
	if hasExitMarker {
		stamp, err := completionFileTime(jobstore.ExitCodePath(dir))
		if !errors.Is(err, fs.ErrNotExist) {
			return stamp, err
		}
	}
	stamp, resultErr := completionFileTime(jobstore.ResultPath(dir))
	if resultErr == nil {
		return stamp, nil
	}
	if completion, ok := jobstore.ReadProcessCompletion(dir); ok {
		return completion.RecordedAt, nil
	}
	if !errors.Is(resultErr, fs.ErrNotExist) {
		return time.Time{}, resultErr
	}
	return m.store.TerminalObservedAt(meta, observe)
}

func completionFileTime(path string) (time.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("completion receipt %q is not a regular file", filepath.Base(path))
	}
	return info.ModTime(), nil
}
