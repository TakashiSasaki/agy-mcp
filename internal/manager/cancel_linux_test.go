package manager

import (
	"os/exec"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/procstat"
)

func TestRequestCancelRechecksIdentityAfterPIDFDOpen(t *testing.T) {
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
	m := newManager(t, managerOpts{supervisorExe: "sleep"})
	meta := jobstore.Meta{PID: pid, BootID: readBootID(), StartTimeTicks: stat.StartTicks}
	if !m.processAlive(meta) {
		t.Fatal("matching live supervisor failed the precheck")
	}
	// Model the process seen after the precheck using another generation
	// at the same PID. Call the Linux signaling path directly.
	meta.StartTimeTicks++
	if err := m.requestCancel(meta); err != nil {
		t.Fatal(err)
	}
	meta.StartTimeTicks = 0
	if err := m.requestCancel(meta); err == nil {
		t.Fatal("signaling without a recorded identity must fail explicitly")
	}
	time.Sleep(30 * time.Millisecond)
	if !m.processAlive(jobstore.Meta{PID: pid, BootID: readBootID(), StartTimeTicks: stat.StartTicks}) {
		t.Fatal("unverified target received a signal")
	}
}
