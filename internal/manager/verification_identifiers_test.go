package manager

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Removing any identifier's truncateUTF8 call must fail its length assertion.
func TestVerificationFailureIdentifiersBounded(t *testing.T) {
	for _, tc := range []struct {
		name, value, want, kind string
	}{
		{"ascii", strings.Repeat("A", 200000), strings.Repeat("A", 2048), "test_error"},
		{"multibyte", strings.Repeat("界", 1000), strings.Repeat("界", 682), "test_error"},
		{"timeout", "subprocess.TimeoutExpired", "subprocess.TimeoutExpired", "subprocess_timeout"},
		{"timeout_prefix", "TimeoutExpired" + strings.Repeat("X", 4096), "TimeoutExpired" + strings.Repeat("X", 2034), "test_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := strings.ReplaceAll(`<testsuite tests="1" errors="1"><testcase name="VALUE" classname="VALUE"><error type="VALUE">detail</error></testcase></testsuite>`, "VALUE", tc.value)
			out := parseJUnit([]byte(report))
			if !out.Complete || out.VerificationOutcome != "failed" || out.Errors != 1 || out.Tests != 1 || len(out.Failures) != 1 {
				t.Fatalf("identifier bounding changed verdict: complete=%t outcome=%s errors=%d tests=%d details=%d", out.Complete, out.VerificationOutcome, out.Errors, out.Tests, len(out.Failures))
			}
			failure := out.Failures[0]
			for field, got := range map[string]string{"test": failure.Test, "class": failure.Class, "reported_type": failure.ReportedType} {
				if len(got) > 2048 || !utf8.ValidString(got) || got != tc.want {
					t.Errorf("%s has %d bytes, want %d with valid UTF-8 and unchanged prefix", field, len(got), len(tc.want))
				}
			}
			if failure.Kind != tc.kind {
				t.Errorf("kind = %s, want %s", failure.Kind, tc.kind)
			}
		})
	}
}
