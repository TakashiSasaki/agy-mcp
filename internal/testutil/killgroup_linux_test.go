package testutil

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/procstat"
	"golang.org/x/sys/unix"
)

func TestGroupWithUnreapedZombieHasStopped(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(-pid, 0); err != nil {
		t.Fatalf("zombie control: kill(-pgid, 0) failed: %v", err)
	}
	KillProcessGroup(t, pid, 100*time.Millisecond)
	if live, err := groupHasLiveMembers(pid); err != nil || live {
		t.Fatalf("unreaped group member is stopped: live=%t, %v", live, err)
	}
}

func TestObserveProcessRejectsRecycledIdentity(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	stat, err := procstat.Read(pid)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := processStillRunning(pid, stat.StartTicks); err != nil || !live {
		t.Fatalf("matching identity should be running: %t, %v", live, err)
	}
	if live, err := processStillRunning(pid, stat.StartTicks+1); err != nil || live {
		t.Fatalf("different start time should be rejected: %t, %v", live, err)
	}
}
