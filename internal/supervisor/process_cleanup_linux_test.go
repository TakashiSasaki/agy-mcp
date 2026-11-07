//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

const cleanupRoleEnv = "AGY_TEST_CLEANUP_ROLE"

func cleanupHelperCommand(role, name, dir string) *exec.Cmd {
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "-test.run=^TestProcessCleanupHelper$")
	cmd.Env = append(os.Environ(), cleanupRoleEnv+"="+role, "AGY_TEST_CLEANUP_NAME="+name, "AGY_TEST_CLEANUP_DIR="+dir)
	return cmd
}

func waitCleanupFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

// Helper invocations run in separate processes so subreaper and signal state
// never affect the test runner or another concurrent job.
func TestProcessCleanupHelper(t *testing.T) {
	role := os.Getenv(cleanupRoleEnv)
	if role == "" {
		return
	}
	dir, name := os.Getenv("AGY_TEST_CLEANUP_DIR"), os.Getenv("AGY_TEST_CLEANUP_NAME")
	switch role {
	case "worker":
		signal.Ignore(syscall.SIGTERM)
		identity, err := readDescendantIdentity(os.Getpid())
		if err != nil {
			os.Exit(91)
		}
		data := fmt.Sprintf("%d %d\n", identity.pid, identity.ticks)
		if err := os.WriteFile(filepath.Join(dir, name+".pid"), []byte(data), 0o600); err != nil {
			os.Exit(92)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "bridge":
		cmd := cleanupHelperCommand("worker", "adopted", dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil || waitCleanupFile(filepath.Join(dir, "adopted.pid"), 3*time.Second) != nil {
			os.Exit(93)
		}
		os.Exit(0) // orphan adopted by the job-specific subreaper
	case "agy":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		for _, worker := range []string{"same-group", "new-session"} {
			cmd := cleanupHelperCommand("worker", worker, dir)
			if worker == "new-session" {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			if err := cmd.Start(); err != nil || waitCleanupFile(filepath.Join(dir, worker+".pid"), 3*time.Second) != nil {
				os.Exit(94)
			}
		}
		bridge := cleanupHelperCommand("bridge", "", dir)
		if err := bridge.Run(); err != nil {
			os.Exit(95)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
			os.Exit(96)
		}
		<-term
		os.Exit(1) // root exits early; descendants deliberately ignore SIGTERM
	case "supervisor":
		cleanup, err := newProcessCleanup()
		if err == nil {
			err = runWithCleanup(dir, 80*time.Millisecond, 100*time.Millisecond, cleanup)
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		os.Exit(0)
	default:
		os.Exit(98)
	}
}

func cleanupRecordedWorker(t *testing.T, path string) (pid int, ticks uint64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscanf(string(data), "%d %d", &pid, &ticks); err != nil {
		t.Fatal(err)
	}
	return pid, ticks
}

// A cached ancestor chain can falsely include an unrelated child after an
// ancestor PID is recycled. Its own identity still matches; current parent
// ownership must be checked independently before the pidfd receives a signal.
func TestCleanupRejectsStaleAncestorOwnership(t *testing.T) {
	dir := t.TempDir()
	outsider := cleanupHelperCommand("worker", "outsider", dir)
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outsider.Process.Kill(); _ = outsider.Wait() }()
	if err := waitCleanupFile(filepath.Join(dir, "outsider.pid"), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	actual, err := readDescendantIdentity(outsider.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Inject a stale ancestry assertion. owner is a live process, but it is
	// not this child's real parent; matching child start ticks are insufficient.
	cleanup := &linuxProcessCleanup{owner: os.Getppid()}
	stale := actual
	stale.parent = cleanup.owner
	if err := cleanup.signalChild(stale, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	current, err := readDescendantIdentity(actual.pid)
	if err != nil || current.ticks != actual.ticks || current.state == "Z" {
		t.Fatalf("stale ancestry permitted signaling unrelated process: current=%+v error=%v", current, err)
	}
}

// Covers deliberate mutation failures without killing reused PIDs.
func killRecordedWorkers(dir string) {
	for _, name := range []string{"same-group", "new-session", "adopted"} {
		data, err := os.ReadFile(filepath.Join(dir, name+".pid"))
		if err != nil {
			continue
		}
		var pid int
		var ticks uint64
		_, _ = fmt.Sscanf(string(data), "%d %d", &pid, &ticks)
		current, err := readDescendantIdentity(pid)
		if err == nil && current.ticks == ticks {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Mutation guards: removing subreaper adoption leaves the double-forked worker;
// removing post-root SIGKILL leaves TERM-ignoring workers; removing the cleanup
// barrier allows a terminal sentinel while descendants are still alive.
func TestRunProcessCleansDetachedDescendants(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			fake := filepath.Join(dir, "fake-agy")
			script := "#!/bin/sh\nexport " + cleanupRoleEnv + "=agy\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestProcessCleanupHelper$\n"
			if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			timeout := 10 * time.Second
			wantCode := jobstore.ExitSIGTERM
			if mode == "timeout" {
				timeout, wantCode = 3*time.Second, jobstore.ExitTimeout
			}
			writeMeta(t, dir, jobstore.Meta{ID: "cleanup", AgyPath: fake, Cwd: dir, Timeout: timeout, StartedAt: time.Now()})
			outsider := cleanupHelperCommand("worker", "unrelated", dir)
			outsider.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := outsider.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = outsider.Process.Kill(); _ = outsider.Wait() }()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			cmd := cleanupHelperCommand("supervisor", "", dir)
			cmd.Cancel = nil
			cmd.WaitDelay = time.Second
			// CommandContext ensures failed assertions cannot leave this helper.
			bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
			bounded.Env, bounded.WaitDelay = cmd.Env, cmd.WaitDelay
			var stderr strings.Builder
			bounded.Stderr = &stderr
			if err := bounded.Start(); err != nil {
				t.Fatal(err)
			}
			defer killRecordedWorkers(dir)
			if err := waitCleanupFile(filepath.Join(dir, "ready"), 6*time.Second); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				if err := bounded.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			if err := waitCleanupFile(jobstore.ExitCodePath(dir), 8*time.Second); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"same-group", "new-session", "adopted"} {
				pid, ticks := cleanupRecordedWorker(t, filepath.Join(dir, name+".pid"))
				current, err := readDescendantIdentity(pid)
				if err == nil && current.ticks == ticks && current.state != "Z" {
					t.Errorf("terminal sentinel preceded cleanup of %s pid=%d", name, pid)
				}
			}
			if err := bounded.Wait(); err != nil {
				t.Fatalf("supervisor %v: %s", err, stderr.String())
			}
			code, err := os.ReadFile(jobstore.ExitCodePath(dir))
			if err != nil || strings.TrimSpace(string(code)) != strconv.Itoa(wantCode) {
				t.Fatalf("exit code %q error %v, want %d", code, err, wantCode)
			}
			pid, ticks := cleanupRecordedWorker(t, filepath.Join(dir, "unrelated.pid"))
			current, err := readDescendantIdentity(pid)
			if err != nil || current.ticks != ticks || current.state == "Z" {
				t.Fatalf("unrelated sibling affected: %+v %v", current, err)
			}
		})
	}
}

func TestRunProcessInitializationFailsBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	err := runProcess(dir, func() (processCleanup, error) { return nil, errors.New("pidfd denied") })
	if err == nil || !strings.Contains(err.Error(), "pidfd denied") {
		t.Fatalf("initialization failure hidden: %v", err)
	}
	data, readErr := os.ReadFile(jobstore.ErrPath(dir))
	if readErr != nil || !strings.Contains(string(data), "pidfd denied") {
		t.Fatalf("missing actionable error: %q %v", data, readErr)
	}
	data, readErr = os.ReadFile(jobstore.ExitCodePath(dir))
	if readErr != nil || strings.TrimSpace(string(data)) != strconv.Itoa(jobstore.ExitSpawnFail) {
		t.Fatalf("missing spawn-failure sentinel: %q %v", data, readErr)
	}
	var process jobstore.ProcessOutcome
	data, readErr = os.ReadFile(filepath.Join(dir, jobstore.ProcessFile))
	if readErr != nil || json.Unmarshal(data, &process) != nil || process.PID != 0 {
		t.Fatalf("AGY should not have spawned: %s %v", data, readErr)
	}
}
