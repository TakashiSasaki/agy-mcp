//go:build linux || darwin

package testutil

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// KillProcessGroup sends SIGKILL to a test-owned process group and waits
// until its members have stopped executing. The caller still owns reaping.
func KillProcessGroup(tb testing.TB, pgid int, timeout time.Duration) {
	tb.Helper()
	if pgid <= 0 {
		tb.Errorf("KillProcessGroup: refusing non-positive pgid %d", pgid)
		return
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		tb.Errorf("KillProcessGroup: kill(-%d, SIGKILL): %v", pgid, err)
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		live, err := groupHasLiveMembers(pgid)
		if err != nil {
			tb.Errorf("inspect process group %d: %v", pgid, err)
			return
		}
		if !live {
			return
		}
		if time.Now().After(deadline) {
			tb.Errorf("process group %d still has live members %v after SIGKILL", pgid, timeout)
			return
		}
		time.Sleep(pollInterval)
	}
}
