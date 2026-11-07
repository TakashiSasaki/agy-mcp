//go:build linux || darwin

package testutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// KillProcessGroup must not return while a member of the group can still run
// (issue #196). The grandchild keeps writing after the group leader is reaped.
func TestKillProcessGroupLeavesNoLiveMember(t *testing.T) {
	writes := filepath.Join(t.TempDir(), "writes")
	cmd := exec.Command("sh", "-c", `(while :; do printf x >> "$1"; sleep 0.02; done) >/dev/null & echo $!`, "sh", writes)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			KillProcessGroup(t, pgid, 10*time.Second)
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})

	var grandchild int
	if _, err := fmt.Fscan(stdout, &grandchild); err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reap the group leader: %v", err)
	}
	observe, err := ObserveProcess(grandchild)
	if err != nil {
		t.Fatalf("observe the writer after leader exit: %v", err)
	}

	var sizeAfterLeaderExit int64
	if info, err := os.Stat(writes); err == nil {
		sizeAfterLeaderExit = info.Size()
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := os.Stat(writes)
		if err == nil && info.Size() >= sizeAfterLeaderExit+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("negative control: the grandchild did not write repeatedly")
		}
		time.Sleep(pollInterval)
	}
	if live, err := observe(); err != nil || !live {
		t.Fatalf("negative control: writer should still be live: %v", err)
	}
	if live, err := groupHasLiveMembers(pgid); err != nil || !live {
		t.Fatalf("negative control: group with a writer and no leader reported stopped: %v", err)
	}
	KillProcessGroup(t, pgid, 10*time.Second)
	if live, err := groupHasLiveMembers(pgid); err != nil || live {
		t.Fatalf("group has a running member after SIGKILL: %v", err)
	}
	stopped = true
	if live, err := observe(); err != nil || live {
		t.Fatalf("writer did not stop: live=%t, %v", live, err)
	}
	before, err := os.ReadFile(writes)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(writes)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("grandchild kept writing after group cleanup: %v", err)
	}
}

// errRecorder captures Errorf calls so a test can assert KillProcessGroup's
// failure branches without failing itself.
type errRecorder struct {
	testing.TB
	errs []string
}

func (r *errRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// A non-positive pgid must be refused before kill(2): kill(-0, ...) would
// signal the caller's own process group. Do not check this test by deleting
// the guard: the resulting kill(0, SIGKILL) kills the test run itself.
func TestKillProcessGroupRefusesNonPositivePgid(t *testing.T) {
	rec := &errRecorder{TB: t}
	KillProcessGroup(rec, 0, time.Second)
	if len(rec.errs) != 1 {
		t.Fatalf("errors = %q, want exactly one refusal", rec.errs)
	}
}

// A running sleeper must count as a live group member.
func TestGroupHasLiveMembersDetectsSleeper(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	if live, err := groupHasLiveMembers(pgid); err != nil || !live {
		t.Fatalf("running control is not detected: %v", err)
	}
}
