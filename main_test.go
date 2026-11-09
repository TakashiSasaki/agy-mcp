package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// builtBin holds the path buildBinary compiled to, so TestMain can remove it
// after the run. It is written inside the buildBinary once-func (which completes
// before any test using the binary returns) and read in TestMain only after
// m.Run, so there is no race.
var builtBin string

// buildBinary compiles the agy-mcp binary once for the whole package test run and
// returns its path; the two run-job tests share it instead of each rebuilding.
// Lazy (built on first use) so a focused run of the pure tests below pays nothing.
// CreateTemp gives a unique path for this one shared binary; t.TempDir/t.ArtifactDir
// are per-test and unavailable here (this is package-level, with no *testing.T),
// so TestMain owns the cleanup instead.
var buildBinary = sync.OnceValues(func() (string, error) {
	// Windows resolves an executable through PATHEXT, so an extensionless file is
	// not runnable there no matter that it is a valid PE binary: exec.Command
	// reports "executable file not found in %PATH%" for it. CreateTemp substitutes
	// the last "*", so the suffix survives.
	pattern := "agy-mcp-*"
	if runtime.GOOS == "windows" {
		pattern = "agy-mcp-*.exe"
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	bin := f.Name()
	_ = f.Close() // go build -o overwrites the placeholder file with the binary
	builtBin = bin
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("build agy-mcp: %w\n%s", err, out)
	}
	// Exec the fresh binary once and discard the result, so the operating system
	// pays its first-exec cost here rather than inside a test waiting on the
	// child's startup. On macOS that cost is code-signature validation of a newly
	// written binary, measured at ~360ms cold against ~10ms warm. No test depends
	// on this for correctness any more: the interrupt tests used to race a 300ms
	// timer against the child installing its signal handler, and now block on the
	// readiness handshake instead (see waitReadyFileEnv). What it still buys is
	// keeping that cold-start cost out of every readiness budget in the package.
	//
	// `wait-job` with no arguments is the cheapest complete run available: it
	// prints usage and exits 2 without touching the state dir or execing agy.
	_ = exec.Command(bin, "wait-job").Run()
	return bin, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.Remove(builtBin)
	}
	os.Exit(code)
}

// Build the binary once and use it as its own supervisor against a fake agy.
func TestRunJobSubcommandEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The fake agy is a bash script (testutil.WriteFakeAgy), which the supervisor
		// cannot exec on Windows. The run-job supervisor path is covered on Windows by
		// internal/supervisor's TestRunCapturesOutputAndExit (fake agy via cmd.exe).
		t.Skip("Unix-specific: bash-script fake agy is not executable on Windows")
	}
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "E2E OK", Exit: 0})

	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(jobstore.MetaPath(jobDir), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run-job: %v\n%s", err, out)
	}
	out, _ := os.ReadFile(jobstore.OutPath(jobDir))
	if strings.TrimSpace(string(out)) != "E2E OK" {
		t.Fatalf("out = %q", out)
	}
}

// Use the real supervisor command with a private pipe, as StartJob does.
func TestRunJobAdmission(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprintf("published=%t", published), func(t *testing.T) {
			dir := t.TempDir()
			store := jobstore.New(dir)
			// A published supervisor should proceed as far as trying this
			// missing executable. An unpublished one must not write job output.
			meta := jobstore.Meta{ID: "admission", AgyPath: filepath.Join(dir, "absent-agy"), Cwd: dir}
			jobDir, err := store.Create(meta)
			if err != nil {
				t.Fatal(err)
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close(); _ = w.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "run-job", jobDir, "--await-admission")
			cmd.Stdin = r
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			if published {
				meta.PID = cmd.Process.Pid
				if err := store.UpdateMeta(meta); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil || ctx.Err() != nil {
				t.Fatalf("want a supervisor failure before the deadline, got %v (context %v)", err, ctx.Err())
			}
			code, present := store.ExitCode(meta.ID)
			if published {
				if !present || code != jobstore.ExitSpawnFail {
					t.Fatalf("published job did not attempt to spawn agy: code=%d present=%t", code, present)
				}
			} else if _, err := os.Stat(jobstore.OutPath(jobDir)); present || !os.IsNotExist(err) {
				t.Fatalf("unpublished job wrote evidence: code=%d present=%t stat=%v", code, present, err)
			}
		})
	}
}

// Cancel end to end: SIGTERM the supervisor subprocess and confirm it forwards
// the signal to agy and writes the SIGTERM sentinel, which Status maps to
// "cancelled".
func TestRunJobCancelViaSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		// This exercises cancel via SIGTERM, the Linux delivery mechanism; Windows
		// delivers cancel through a sentinel file instead, covered by internal/supervisor's
		// TestRunCancelViaSentinel. It also uses the bash-script fake agy.
		t.Skip("Unix-specific: cancel-via-SIGTERM and bash-script fake agy do not apply on Windows")
	}
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	// MEASURED against agy 1.5.3: SIGTERM can be handled as an ordinary exit 1.
	// The supervisor must preserve both that raw code and the cancel verdict.
	agy := filepath.Join(jobDir, "fake-agy")
	script := "#!/bin/sh\ntrap 'exit 1' TERM\nprintf 'ready\\n' >&2\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(agy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}, Timeout: 30 * time.Second}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(jobstore.MetaPath(jobDir), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// On any fatal path below (before the happy-path Wait), tear the supervisor
	// down so neither it nor its fake agy child leaks. SIGTERM lets the
	// supervisor forward the signal to agy and reap it, unlike a bare Kill.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})

	// Wait for the child to arm its trap before signalling the supervisor.
	testutil.WaitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(jobstore.ErrPath(jobDir))
		return err == nil && strings.Contains(string(data), "ready\n")
	}, "fake agy did not arm its SIGTERM trap")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true

	testutil.WaitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(jobstore.ExitCodePath(jobDir))
		return err == nil
	}, "supervisor did not write exit_code after SIGTERM")
	code, _ := os.ReadFile(jobstore.ExitCodePath(jobDir))
	if strings.TrimSpace(string(code)) != strconv.Itoa(jobstore.ExitSIGTERM) {
		t.Fatalf("exit_code = %q, want %d (SIGTERM cancel)", code, jobstore.ExitSIGTERM)
	}
	receipt, err := jobstore.ReadProcessDir(jobDir)
	if err != nil || receipt == nil || !receipt.Cancelled || receipt.ExitCode == nil || *receipt.ExitCode != 1 {
		t.Fatalf("process receipt = %+v, err = %v; want cancellation with raw exit 1", receipt, err)
	}
}

func TestCheckLoopbackAddr(t *testing.T) {
	// Literal IPs and localhost (resolved via the real /etc/hosts) are loopback;
	// hostname resolution corner cases are covered hermetically below.
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765", "127.0.0.1:0", "[::1]"} {
		if err := checkLoopbackAddr(addr); err != nil {
			t.Errorf("checkLoopbackAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{":8765", "0.0.0.0:8765", "192.168.1.10:8765"} {
		if err := checkLoopbackAddr(addr); err == nil {
			t.Errorf("checkLoopbackAddr(%q) = nil, want error", addr)
		}
	}
}

func TestCheckLoopbackAddrResolvesHostnames(t *testing.T) {
	loopback := func(string) ([]string, error) { return []string{"127.0.0.1", "::1"}, nil }
	remapped := func(string) ([]string, error) { return []string{"127.0.0.1", "10.0.0.5"}, nil }
	failing := func(string) ([]string, error) { return nil, errors.New("no such host") }
	empty := func(string) ([]string, error) { return nil, nil }

	if err := checkLoopbackAddrResolved("localhost:8765", loopback); err != nil {
		t.Errorf("localhost resolving to loopback should pass, got %v", err)
	}
	// A hosts-file remap of localhost to a routable IP must be rejected even though
	// one resolved address is still loopback: any non-loopback address exposes it.
	if err := checkLoopbackAddrResolved("localhost:8765", remapped); err == nil {
		t.Error("localhost remapped to a non-loopback address must be rejected")
	}
	if err := checkLoopbackAddrResolved("localhost:8765", failing); err == nil {
		t.Error("a resolve failure must be rejected, not silently allowed")
	}
	if err := checkLoopbackAddrResolved("localhost:8765", empty); err == nil {
		t.Error("a host resolving to no addresses must be rejected")
	}
	// A literal IP needs no resolution and must not call the resolver.
	called := false
	spy := func(string) ([]string, error) { called = true; return nil, nil }
	if err := checkLoopbackAddrResolved("127.0.0.1:8765", spy); err != nil {
		t.Errorf("literal loopback IP should pass, got %v", err)
	}
	if called {
		t.Error("a literal IP must not trigger a hostname lookup")
	}
}

func TestVersionSubcommandReportsSemVerBuildMetadata(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"version", "--version", "-version"} {
		out, err := exec.Command(bin, arg).CombinedOutput()
		if err != nil {
			t.Fatalf("agy-mcp %s failed: %v\n%s", arg, err, out)
		}
		line := strings.TrimSpace(string(out))
		if !strings.HasPrefix(line, "agy-mcp v2.11.0+local.") {
			t.Errorf("agy-mcp %s = %q, want prefix \"agy-mcp v2.11.0+local.\"", arg, line)
		}
	}
}
