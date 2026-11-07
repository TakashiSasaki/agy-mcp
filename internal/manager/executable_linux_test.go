//go:build linux

package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Mutation guard: bypassing os.SameFile or the missing-path branch must fail
// the corresponding cases. Errors must not claim a current installation.
func TestExecutableStateEvidence(t *testing.T) {
	dir := t.TempDir()
	infos := make([]os.FileInfo, 0, 2)
	for _, name := range []string{"running", "replacement"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}
	for _, tc := range []struct {
		name, state               string
		executableErr, runningErr error
		installedErr              error
		installed                 os.FileInfo
	}{
		{name: "same inode", state: "current", installed: infos[0]},
		{name: "replacement", state: "replaced", installed: infos[1]},
		{name: "missing path", state: "missing", installedErr: os.ErrNotExist},
		{name: "unavailable proc", state: "unknown", runningErr: os.ErrNotExist},
		{name: "permission denied", state: "unknown", installedErr: os.ErrPermission},
		{name: "unknown path", state: "unknown", executableErr: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, advice := inspectExecutableState(func() (string, error) { return "/installed", tc.executableErr }, func(path string) (os.FileInfo, error) {
				if path == "/proc/self/exe" {
					return infos[0], tc.runningErr
				}
				if path != "/installed" {
					t.Fatalf("unexpected probe %q", path)
				}
				return tc.installed, tc.installedErr
			})
			if state != tc.state || (advice != "") != (state == "replaced" || state == "missing") {
				t.Fatalf("state=%q advice=%q, want %q", state, advice, tc.state)
			}
		})
	}
}

// This is a child-process helper, not a production server or a model turn.
func TestExecutableStateChild(t *testing.T) {
	if os.Getenv("AGY_MCP_EXECUTABLE_TEST_CHILD") != "1" {
		return
	}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		state, _ := executableState()
		fmt.Println(state)
	}
	os.Exit(0)
}

// The same persistent process observes an atomic replacement and then removal
// of its own executable. Mutation guard: returning current unconditionally or
// comparing the installed path with itself must make this test fail.
func TestExecutableStateLiveReplacement(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	installed := filepath.Join(t.TempDir(), "agy-test")
	output, err := os.OpenFile(installed, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy binary: %v, %v", copyErr, closeErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, installed, "-test.run=^TestExecutableStateChild$")
	cmd.Env = append(os.Environ(), "AGY_MCP_EXECUTABLE_TEST_CHILD=1")
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("child: %v (%s)", err, stderr.String())
		}
	}()
	scanner := bufio.NewScanner(stdout)
	check := func(want string) {
		t.Helper()
		if _, err := io.WriteString(stdin, "check\n"); err != nil {
			t.Fatal(err)
		}
		if !scanner.Scan() || scanner.Text() != want {
			t.Fatalf("child state=%q want=%q (scanner %v)", scanner.Text(), want, scanner.Err())
		}
	}
	check("current")
	replacement := installed + ".new"
	if err := os.WriteFile(replacement, []byte("replacement inode; never executed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatal(err)
	}
	check("replaced")
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	check("missing")
}
