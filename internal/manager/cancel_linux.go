package manager

import (
	"errors"
	"fmt"
	"os"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/procstat"
	"golang.org/x/sys/unix"
)

func (m *Manager) requestCancel(meta jobstore.Meta) error {
	if meta.PID <= 0 || meta.BootID != readBootID() {
		return nil
	}
	if meta.StartTimeTicks == 0 {
		return fmt.Errorf("supervisor start time unavailable; cannot verify PID %d for cancellation", meta.PID)
	}
	fd, err := unix.PidfdOpen(meta.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open supervisor pidfd: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	stat, err := procstat.Read(meta.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify supervisor identity: %w", err)
	}
	if stat.Exited() || stat.StartTicks != meta.StartTimeTicks {
		return nil
	}
	// The descriptor remains bound to the process opened above, even if its
	// numeric PID is recycled after the identity check.
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}
