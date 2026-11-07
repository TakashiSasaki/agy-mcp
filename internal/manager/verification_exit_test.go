package manager

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Removing the exit-code override must fail the enclosing-command case.
func TestVerificationCommandExit(t *testing.T) {
	m, job, root := setupTestJobWithFiles(t)
	report := `<testsuite><testcase name="child"/></testsuite>`
	for _, tc := range []struct {
		name, report, exit, outcome, original string
		code                                  int32
	}{
		{"success", report, "0\n", "passed", "passed", 0},
		{"enclosing command failed", report, "2\n", "failed", "passed", 2},
		{"signal", report, "-9\n", "failed", "passed", -9},
		{"plus zero", report, "+0\n", "passed", "passed", 0},
		{"failed test successful command", `<testsuite><testcase><failure/></testcase></testsuite>`, "0", "failed", "failed", 0},
		{"incomplete report failed command", `<testsuite>`, "2", "unknown", "unknown", 2},
		{"minimum status", report, "-2147483648", "failed", "passed", -2147483648},
		{"maximum status", report, "2147483647", "failed", "passed", 2147483647},
		{"64-byte status", report, strings.Repeat(" ", 62) + "0\n", "passed", "passed", 0},
		{"incomplete report successful command", `<testsuite>`, "0", "unknown", "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, data := range map[string]string{"tests.xml": tc.report, "command.exit": tc.exit} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := m.VerificationWithExitCode(job, "tests.xml", "junit", "command.exit")
			if err != nil {
				t.Fatal(err)
			}
			if out.VerificationOutcome != tc.outcome || out.ReportOutcome != tc.original || out.CommandExit == nil {
				t.Fatalf("exit evidence lost: %+v", out)
			}
			want := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.exit)))
			if out.CommandExit.SHA256 != want || out.CommandExit.Source != "exit_code_artifact" || out.CommandExit.ExitCode != tc.code || out.CommandExit.Path != "command.exit" {
				t.Fatalf("wrong receipt identity: %+v", out.CommandExit)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "tests.xml"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := m.VerificationWithExitCode(job, "tests.xml", "junit", "")
	if err != nil || legacy.VerificationOutcome != "passed" || legacy.CommandExit != nil || legacy.ReportOutcome != "" {
		t.Fatalf("legacy behavior changed: %+v %v", legacy, err)
	}
	for _, data := range []string{"", "0\n2", "null", "true", "0.0", "2147483648", "-2147483649", "\xff", strings.Repeat(" ", 63) + "0\n"} {
		if err := os.WriteFile(filepath.Join(root, "command.exit"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.VerificationWithExitCode(job, "tests.xml", "junit", "command.exit"); err == nil {
			t.Fatalf("invalid receipt accepted: %q", data)
		}
	}
	for _, path := range []string{"missing.exit", "../outside.exit", "."} {
		if _, err := m.VerificationWithExitCode(job, "tests.xml", "junit", path); err == nil {
			t.Fatalf("unavailable receipt accepted: %s", path)
		}
	}
}
