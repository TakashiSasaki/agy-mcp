package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/procstat"
)

// startTimeMandatory reports whether StartJob must fail when it cannot record a
// supervisor start time. It is false wherever processAlive has a name-based
// liveness fallback (Linux /proc/comm, Windows image name) or never runs a live
// job (the unsupported stub); only darwin (no fallback) sets it true.
const startTimeMandatory = false

// bootID reads the kernel boot id once and caches it. The value is stable for
// the lifetime of the process, so liveness checks (which run per poll) do not
// re-read /proc each time.
var bootID = sync.OnceValue(func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
})

func readBootID() string { return bootID() }

// processAlive reports whether the job's recorded supervisor PID is still a
// live supervisor process from the current boot.
func (m *Manager) processAlive(meta jobstore.Meta) bool {
	return m.processAliveWithStat(meta, procstat.Read)
}

func (m *Manager) processAliveWithStat(meta jobstore.Meta, readStat func(int) (procstat.Stat, error)) bool {
	if meta.PID <= 0 || meta.BootID != readBootID() {
		return false
	}
	if err := syscall.Kill(meta.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	stat, err := readStat(meta.PID)
	if err == nil {
		if stat.Exited() {
			return false
		}
		if meta.StartTimeTicks != 0 {
			return stat.StartTicks == meta.StartTimeTicks
		}
		return stat.Comm == expectedComm(m.cfg.SupervisorExe)
	}
	// An unavailable stat is not evidence of death. Keep a recorded identity
	// live while the PID exists, even during the fork-to-exec name change.
	if meta.StartTimeTicks != 0 {
		return true
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(meta.PID) + "/comm")
	return err != nil || strings.TrimSpace(string(comm)) == expectedComm(m.cfg.SupervisorExe)
}

func readStartTimeTicks(pid int) (uint64, bool) {
	stat, err := procstat.Read(pid)
	return stat.StartTicks, err == nil
}

// expectedComm returns the process name as the kernel records it in
// /proc/<pid>/comm for the given supervisor executable: the basename truncated
// to the kernel's 15-character comm limit.
func expectedComm(exe string) string {
	base := filepath.Base(exe)
	if len(base) > 15 {
		base = base[:15]
	}
	return base
}
