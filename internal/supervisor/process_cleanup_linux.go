//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"syscall"

	"github.com/tphakala/agy-mcp/v2/internal/procstat"
	"golang.org/x/sys/unix"
)

type descendantIdentity struct {
	pid, parent int
	ticks       uint64
	state       string
	depth       int
}

type linuxProcessCleanup struct{ owner int }

func newProcessCleanup() (processCleanup, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd capability required for descendant cleanup: %w", err)
	}
	probeErr := unix.PidfdSendSignal(fd, 0, nil, 0)
	_ = unix.Close(fd)
	if probeErr != nil {
		return nil, fmt.Errorf("pidfd signaling capability required: %w", probeErr)
	}
	cleanup := &linuxProcessCleanup{owner: os.Getpid()}
	if _, err := readDescendantIdentity(cleanup.owner); err != nil {
		return nil, err
	}
	if _, err := cleanup.descendants(); err != nil {
		return nil, err
	}
	return cleanup, nil
}

func readDescendantIdentity(pid int) (descendantIdentity, error) {
	stat, err := procstat.Read(pid)
	if err != nil {
		return descendantIdentity{}, err
	}
	return descendantIdentity{pid: pid, parent: stat.Parent, ticks: stat.StartTicks, state: string(stat.State)}, nil
}

func (c *linuxProcessCleanup) descendants() ([]descendantIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := make(map[int]descendantIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == c.owner {
			continue
		}
		identity, err := readDescendantIdentity(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			// Unrelated users' processes may be inaccessible. Own descendants
			// share this supervisor's credentials in the supported workflow.
			if errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		all[pid] = identity
	}
	owned := map[int]int{c.owner: 0}
	var result []descendantIdentity
	for {
		added := false
		for pid, identity := range all {
			depth, found := owned[identity.parent]
			if !found {
				continue
			}
			identity.depth = depth + 1
			owned[pid] = identity.depth
			result = append(result, identity)
			delete(all, pid)
			added = true
		}
		if !added {
			break
		}
	}
	// Keep observation order stable. Signals separately revalidate direct
	// ownership; later scans catch descendants adopted after parent exit.
	slices.SortFunc(result, func(a, b descendantIdentity) int { return b.depth - a.depth })
	return result, nil
}

func (c *linuxProcessCleanup) signal(sig syscall.Signal) error {
	children, err := c.descendants()
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := c.signalChild(child, sig); err != nil {
			return err
		}
	}
	return nil
}

func (c *linuxProcessCleanup) signalChild(child descendantIdentity, sig syscall.Signal) error {
	if child.state == "Z" {
		return nil
	}
	// A snapshot's PPID chain alone cannot establish ownership: an ancestor
	// PID may have been recycled. Only signal a currently direct child of
	// this still-live supervisor; subreaper adoption brings deeper children
	// here after their parents exit. Bind the signal to a pidfd before checking
	// ticks and current parent, so PID reuse cannot redirect the later signal.
	fd, err := unix.PidfdOpen(child.pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open descendant pidfd %d: %w", child.pid, err)
	}
	current, readErr := readDescendantIdentity(child.pid)
	if readErr == nil && current.ticks == child.ticks && current.state != "Z" && current.parent == c.owner {
		err = unix.PidfdSendSignal(fd, sig, nil, 0)
	}
	_ = unix.Close(fd)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, syscall.ESRCH) {
		return readErr
	}
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (c *linuxProcessCleanup) remaining() (bool, error) {
	children, err := c.descendants()
	if err != nil {
		return false, err
	}
	for _, child := range children {
		if child.state != "Z" {
			return true, nil
		}
	}
	return false, nil
}

// reapAdopted runs while cmd.Wait owns the root PID. Wait4 only targets
// exited direct children, and the supervisor is their sole waiter.
func (c *linuxProcessCleanup) reapAdopted(rootPID int) error {
	children, err := c.descendants()
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.pid == rootPID || child.parent != c.owner || child.state != "Z" {
			continue
		}
		_, err := unix.Wait4(child.pid, nil, unix.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
	return nil
}

func (*linuxProcessCleanup) reap() error {
	for {
		pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			return nil
		}
		if err != nil {
			return err
		}
		if pid == 0 {
			return nil
		}
	}
}
