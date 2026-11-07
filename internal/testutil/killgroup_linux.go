package testutil

import (
	"errors"
	"os"
	"strconv"
	"syscall"

	"github.com/tphakala/agy-mcp/v2/internal/procstat"
)

func groupHasLiveMembers(pgid int) (bool, error) {
	if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := procstat.Read(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		if stat.Group == pgid && !stat.Exited() {
			return true, nil
		}
	}
	return false, nil
}

// ObserveProcess tracks the process currently using pid until it stops running.
func ObserveProcess(pid int) (func() (bool, error), error) {
	stat, err := procstat.Read(pid)
	if err != nil {
		return nil, err
	}
	return func() (bool, error) { return processStillRunning(pid, stat.StartTicks) }, nil
}

func processStillRunning(pid int, startTicks uint64) (bool, error) {
	stat, err := procstat.Read(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return err == nil && stat.StartTicks == startTicks && !stat.Exited(), err
}
