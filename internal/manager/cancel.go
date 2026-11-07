package manager

import (
	"fmt"
)

// Cancel asks the job's supervisor to terminate its agy child. The supervisor
// then terminates agy and writes the exit-code sentinel, so the job ends in a
// definite "cancelled" state. If the supervisor is no longer alive (already
// exited, or a stale PID), Cancel is a no-op success and Status reports the
// terminal state from disk.
//
// How the request reaches the supervisor is platform-specific (requestCancel):
// Linux sends SIGTERM through a verified pidfd. Darwin uses a PID signal.
// Windows writes a cancel sentinel file the supervisor polls for.
func (m *Manager) Cancel(id string) error {
	meta, err := m.store.Load(id)
	if err != nil {
		return fmt.Errorf("load job %s to cancel: %w", id, err)
	}
	// Filter jobs that have already stopped. Linux requestCancel independently
	// verifies identity after opening the pidfd used to send the signal.
	if !m.processAlive(meta) {
		return nil
	}
	if err := m.requestCancel(meta); err != nil {
		return fmt.Errorf("request supervisor cancel: %w", err)
	}
	return nil
}
