package manager

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// Mutation guard: removing the sequence/plan validation must fail the malformed
// report cases; removing failure accumulation must fail the failing report case.
func TestVerificationFlatTAP(t *testing.T) {
	for _, tc := range []struct {
		name, report, outcome   string
		passed, failed, skipped int
	}{
		{"pass", "1..2\nok 1 - one\nok 2 - two\n", "passed", 2, 0, 0},
		{"fail", "1..2\nok 1 - good\nnot ok 2 - bad\n", "failed", 1, 1, 0},
		{"trailing plan", "TAP version 13\nok 1\n# diagnostic\n1..1\n", "passed", 1, 0, 0},
		{"skip", "1..2\nok 1 - pass\nnot ok 2 - platform # SKIP unavailable\n", "passed", 1, 0, 1},
		// Empty description before SKIP: https://testanything.org/tap-version-13-specification.html#skipping-a-few
		{"skip empty description", "1..2\nok 1\nok 2 - # SKIP platform\n", "passed", 1, 0, 1},
		{"crlf", "1..1\r\nok 1 - pass\r\n", "passed", 1, 0, 0},
		{"empty", "", "unknown", 0, 0, 0},
		{"no plan", "ok 1\n", "unknown", 1, 0, 0},
		{"truncated", "1..2\nok 1\n", "unknown", 1, 0, 0},
		{"extra cases", "1..1\nok 1\nok 2\n", "unknown", 2, 0, 0},
		{"duplicate number", "1..2\nok 1\nok 1\n", "unknown", 2, 0, 0},
		{"out of order", "1..2\nok 2\nok 1\n", "unknown", 2, 0, 0},
		{"duplicate plan", "1..1\nok 1\n1..1\n", "unknown", 1, 0, 0},
		{"middle plan", "ok 1\n1..2\nok 2\n", "unknown", 2, 0, 0},
		{"skip only", "1..1\nok 1 # SKIP not supported\n", "unknown", 0, 0, 1},
		{"zero cases", "1..0\n", "unknown", 0, 0, 0},
		{"bailout", "1..1\nok 1\nBail out! interrupted\n", "unknown", 1, 0, 0},
		{"todo unsupported", "1..1\nnot ok 1 # TODO later\n", "unknown", 0, 0, 0},
		{"unknown directive", "1..1\nok 1 # custom\n", "unknown", 0, 0, 0},
		{"nested subtest", "1..1\n# Subtest: child\n    not ok 1\nok 1\n", "unknown", 1, 0, 0},
		{"yaml diagnostics", "1..1\nok 1\n  ---\n  message: invalid\n  ...\n", "unknown", 1, 0, 0},
		{"junk", "1..1\nok 1\nprocess failed\n", "unknown", 1, 0, 0},
		{"invalid utf8", "1..1\nok 1 - \xff\n", "unknown", 1, 0, 0},
		{"late version", "1..1\nTAP version 13\nok 1\n", "unknown", 1, 0, 0},
		{"unsupported version", "TAP version 14\n1..1\nok 1\n", "unknown", 1, 0, 0},
		{"plan overflow", "1..999999999999999999999999999\nok 1\n", "unknown", 1, 0, 0},
		{"number overflow", "1..1\nok 99999999999999999999999999\n", "unknown", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseFlatTAP([]byte(tc.report))
			if out.VerificationOutcome != tc.outcome || out.Passed != tc.passed || out.Failed != tc.failed || out.Skipped != tc.skipped || out.Complete != (tc.outcome != "unknown") {
				t.Fatalf("unexpected TAP verdict: %+v", out)
			}
		})
	}
}

func TestVerificationFlatTAPMeasuredCReport(t *testing.T) {
	data, err := os.ReadFile("testdata/tap-verification/c-bytehist.tap")
	if err != nil {
		t.Fatal(err)
	}
	out := parseFlatTAP(data)
	if out.VerificationOutcome != "passed" || !out.Complete || out.Tests != 10 || out.Passed != 10 || out.Source != "tap_artifact" {
		t.Fatalf("native C report: %+v", out)
	}
}

func TestVerificationFlatTAPFailureBounds(t *testing.T) {
	var report strings.Builder
	report.WriteString("1..101\n")
	for i := 1; i <= 101; i++ {
		fmt.Fprintf(&report, "not ok %d - %s\n", i, strings.Repeat("界", 1000))
	}
	out := parseFlatTAP([]byte(report.String()))
	if out.VerificationOutcome != "failed" || out.Failed != 101 || len(out.Failures) != 100 || !out.FailuresTruncated {
		t.Fatalf("failure limits: %+v", out)
	}
	for _, failure := range out.Failures {
		if len(failure.Test) > 2048 || !utf8.ValidString(failure.Test) || failure.Kind != "test_failure" {
			t.Fatalf("invalid failure: %+v", failure)
		}
	}
}
