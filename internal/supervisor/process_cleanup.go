package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// processCleanup belongs to a dedicated, single-job supervisor process. It
// must never be initialized in the long-lived manager or an embedding caller.
const processOutcomeSource = "supervisor"

type processCleanup interface {
	signal(syscall.Signal) error
	remaining() (bool, error)
	reap() error
}

// awaitAdmission waits for the manager to close its side of the launch pipe.
// The same EOF is delivered if the manager dies. In either case only a job
// whose supervisor PID was published can execute.
func awaitAdmission(jobDir string, gate io.Reader) error {
	var b [1]byte
	if n, err := gate.Read(b[:]); n != 0 || err != io.EOF {
		return errors.New("supervisor admission pipe did not close cleanly")
	}
	meta, err := jobstore.LoadDir(jobDir)
	if err != nil {
		return err
	}
	if meta.PID != os.Getpid() {
		return fmt.Errorf("supervisor PID was not published for job %s", meta.ID)
	}
	return nil
}

// RunProcess is the run-job process entrypoint. On Linux it adopts orphaned
// descendants so cancellation can reach children that created new sessions.
// Run remains available to in-process callers without process-wide adoption.
func RunProcess(jobDir string, gate io.Reader) error {
	if gate != nil {
		if err := awaitAdmission(jobDir, gate); err != nil {
			return err
		}
	}
	return runProcess(jobDir, newProcessCleanup)
}

func runProcess(jobDir string, initialize func() (processCleanup, error)) error {
	cleanup, err := initialize()
	if err != nil {
		message := fmt.Sprintf("initialize supervisor descendant cleanup: %v", err)
		_ = os.WriteFile(jobstore.ErrPath(jobDir), []byte(message+"\n"), 0o600)
		_ = jobstore.WriteProcessDir(jobDir, jobstore.ProcessOutcome{Source: processOutcomeSource, EndedAt: time.Now().UTC()})
		_ = jobstore.WriteExitCodeDir(jobDir, jobstore.ExitSpawnFail)
		return fmt.Errorf("%s", message)
	}
	return runWithCleanup(jobDir, killGrace, drainGrace, cleanup)
}

// finishProcessCleanup keeps escalation alive after the AGY root exits. Only
// reap after its cmd.Wait completes, so wait4 cannot steal the root's status.
func finishProcessCleanup(cleanup processCleanup, rootDone <-chan struct{}, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	emptyScans := 0
	for time.Now().Before(deadline) {
		// Adopted children receive TERM too, after their parent exits.
		if err := cleanup.signal(syscall.SIGTERM); err != nil {
			return err
		}
		if closed(rootDone) {
			if err := cleanup.reap(); err != nil {
				return err
			}
		}
		live, err := cleanup.remaining()
		if err != nil {
			return err
		}
		if !live && closed(rootDone) {
			emptyScans++
			if emptyScans == 2 {
				return nil
			}
		} else {
			emptyScans = 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Re-scan during the bounded kill phase to catch newly adopted descendants.
	deadline = time.Now().Add(2 * time.Second)
	for {
		if err := cleanup.signal(syscall.SIGKILL); err != nil {
			return err
		}
		if closed(rootDone) {
			if err := cleanup.reap(); err != nil {
				return err
			}
		}
		live, err := cleanup.remaining()
		if err != nil {
			return err
		}
		if !live && closed(rootDone) {
			emptyScans++
			if emptyScans == 2 {
				return nil
			}
		} else {
			emptyScans = 0
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("descendants remained alive after bounded SIGKILL cleanup")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
