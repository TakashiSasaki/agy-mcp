package manager

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/procstat"
	"golang.org/x/sys/unix"
)

func TestZombieSupervisorDoesNotHoldJobOrGate(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	ticks, ok := readStartTimeTicks(pid)
	if !ok || unix.Kill(pid, 0) != nil {
		t.Fatal("fixture must be a signalable, unreaped process")
	}
	m := newManager(t, managerOpts{jobTTL: time.Hour})
	meta := jobstore.Meta{ID: "zombie", PID: pid, BootID: readBootID(),
		StartTimeTicks: ticks, ConversationID: "conversation", StartedAt: time.Now()}
	if _, err := m.store.Create(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreAndCollect(); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(meta.ID)
	if err != nil || st.State != StateFailed || st.FailureReason != ReasonInterrupted {
		t.Fatalf("zombie status = %+v, %v", st, err)
	}
	if m.gate.hasInFlight() {
		t.Fatal("unreaped supervisor retained a conversation gate slot")
	}
	end, err := m.store.TerminalObservedAt(meta, false)
	if err != nil || !m.gcEvaluate(m.loadJob(meta.ID), end.Add(time.Nanosecond)) {
		t.Fatalf("zombie job is not collectable after observation: %v", err)
	}
}

func TestExpectedComm(t *testing.T) {
	if got := expectedComm("/usr/local/bin/agy-mcp"); got != "agy-mcp" {
		t.Errorf("expectedComm basename = %q, want agy-mcp", got)
	}
	// Kernel /proc/comm is capped at 15 chars; a longer basename is truncated.
	if got := expectedComm("/x/agy-mcp-v1.2.3-linux-amd64"); got != "agy-mcp-v1.2.3-" {
		t.Errorf("expectedComm truncation = %q (len %d)", got, len(got))
	}
}

func TestParseStartTimeTicks(t *testing.T) {
	// comm is wrapped in a single outer paren pair but may itself contain spaces
	// and parens; parsing must key off the LAST ')'. starttime is field 22, which
	// is index 19 of the whitespace-split remainder after that ')'.
	const stat = "1234 (odd) proc (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 8888888 0 0 0 0"
	got, err := procstat.Parse([]byte(stat))
	if err != nil || got.StartTicks != 8888888 || got.State != 'S' || got.Comm != "odd) proc (x" {
		t.Fatalf("Parse = %+v, %v", got, err)
	}

	// Malformed input returns (0, false), never a misread.
	for _, bad := range []string{"", "no parens here", "123 (x) S 1 2 3"} {
		if _, err := procstat.Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) accepted malformed input", bad)
		}
	}
}

func TestProcessAliveStartTimeCheck(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := New(config.Config{SupervisorExe: exePath, StateDir: t.TempDir(), MaxConcurrency: 4})

	actual, ok := readStartTimeTicks(pid)
	if !ok {
		t.Fatalf("could not read start time for live pid %d", pid)
	}
	base := jobstore.Meta{PID: pid, BootID: readBootID()}

	// Recorded start time matches the live process: alive.
	match := base
	match.StartTimeTicks = actual
	if !m.processAlive(match) {
		t.Error("matching start time should read as alive")
	}

	// Recorded start time differs (a same-boot recycled pid now owned by another
	// agy-mcp would pass the comm check but fail here): not our process.
	mismatch := base
	mismatch.StartTimeTicks = actual + 1
	if m.processAlive(mismatch) {
		t.Error("mismatched start time should read as not alive")
	}

	// A matching (boot id, pid, starttime) triple pins the exact process, so it
	// must read as alive even when the comm does not match the supervisor name.
	// This is the fork-to-exec window: comm is inherited from the parent (or is
	// briefly the script interpreter's) before exec sets the final name.
	wrongComm := New(config.Config{SupervisorExe: "/bogus/not-this-process", StateDir: t.TempDir(), MaxConcurrency: 4})
	if !wrongComm.processAlive(match) {
		t.Error("matching start time should be authoritative over a mismatched comm")
	}

	readFailed := func(int) (procstat.Stat, error) { return procstat.Stat{}, os.ErrPermission }
	if !wrongComm.processAliveWithStat(match, readFailed) {
		t.Error("unreadable stat must not mark a live recorded PID dead")
	}

	// Unknown (0) start time: the check is skipped, preserving prior behavior.
	unknown := base
	unknown.StartTimeTicks = 0
	if !m.processAlive(unknown) {
		t.Error("zero start time should skip the check and read as alive")
	}
}

func TestProcessAliveEmptyBootIDFailsClosed(t *testing.T) {
	if readBootID() == "" {
		// boot_id is unreadable host-wide here (e.g. a restricted container). With no
		// current boot to compare against, an empty recorded boot id correctly falls
		// through to the PID and start-time checks rather than failing closed, so this
		// test's premise does not hold. Skip rather than assert a false expectation.
		t.Skip("boot_id unreadable in this environment; empty-vs-empty correctly falls through")
	}
	pid, exePath := startFakeLiveSupervisor(t)
	m := New(config.Config{SupervisorExe: exePath, StateDir: t.TempDir(), MaxConcurrency: 4})
	actual, ok := readStartTimeTicks(pid)
	if !ok {
		t.Fatalf("could not read start time for live pid %d", pid)
	}
	// An empty recorded boot id cannot be confirmed to belong to the current boot
	// (boot_id was unreadable when the job started). After a reboot a recycled pid
	// could otherwise pass the kill and start-time checks and read as alive, keeping
	// the dead job uncollectable and its gate key held forever. Fail closed: an empty
	// boot id reads as not alive even for a live, exactly-matching pid.
	meta := jobstore.Meta{PID: pid, BootID: "", StartTimeTicks: actual}
	if m.processAlive(meta) {
		t.Error("empty BootID must fail closed (read as not alive)")
	}
}

func TestReadStartTimeTicks(t *testing.T) {
	// Our own pid has a readable, non-zero start time.
	got, ok := readStartTimeTicks(os.Getpid())
	if !ok || got == 0 {
		t.Fatalf("readStartTimeTicks(self) = %d,%v, want non-zero,true", got, ok)
	}
	// A pid that cannot exist yields (0, false), not a panic or a misread.
	if v, ok := readStartTimeTicks(1 << 30); ok || v != 0 {
		t.Errorf("readStartTimeTicks(bogus) = %d,%v, want 0,false", v, ok)
	}
}
