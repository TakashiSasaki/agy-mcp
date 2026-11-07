package jobstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// TerminalObservedAt reads or creates the durable first-observed-dead marker.
// Exclusive creation never follows or overwrites an existing symlink. Reading
// uses Lstat and requires a regular file; later reads never reset its timestamp.
// The caller must establish that the job is not live before requesting creation.
func (s *Store) TerminalObservedAt(meta Meta, create bool) (time.Time, error) {
	if !validJobID(meta.ID) {
		return time.Time{}, ErrInvalidID
	}
	// A stale prepublication (PID 0) observer must never timestamp the later
	// supervisor. Each process generation has its own exclusive marker.
	path := filepath.Join(s.jobDir(meta.ID), fmt.Sprintf("%s.%d.%d", TerminalObservedFile, meta.PID, meta.StartTimeTicks))
	if create {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			info, statErr := f.Stat()
			closeErr := f.Close()
			if statErr != nil {
				return time.Time{}, statErr
			}
			if closeErr != nil {
				return time.Time{}, closeErr
			}
			return info.ModTime(), nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return time.Time{}, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("terminal observation marker is not a regular file")
	}
	return info.ModTime(), nil
}
