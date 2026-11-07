package manager

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// MEASURED from Perl 5.38.2 Test::More 1.302194: a literal hash in a
// description is escaped, including when followed by the word SKIP.
// Mutation guard: replacing cutUnescapedHash with strings.Cut makes failures pass.
func TestVerificationFlatTAPDirectiveEscaping(t *testing.T) {
	for _, tc := range []struct {
		name, report, outcome   string
		passed, failed, skipped int
	}{
		// Native bug reproduction as inline string
		{
			name:    "literal escaped hash failure",
			report:  "1..2\nok 1 - real pass\nnot ok 2 - literal \\# SKIP failure\n",
			outcome: "failed",
			passed:  1,
			failed:  1,
			skipped: 0,
		},
		// Escaped hash in ok test description is not a directive
		{
			name:    "literal escaped hash pass",
			report:  "1..1\nok 1 - literal \\# SKIP passed\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 0,
		},
		// 0 backslashes before #: unescaped directive (SKIP)
		{
			name:    "zero backslashes ok skip",
			report:  "1..2\nok 1 - pass\nok 2 - desc # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "zero backslashes not ok skip",
			report:  "1..2\nok 1 - pass\nnot ok 2 - desc # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		// 1 backslash before #: odd -> escaped hash, not a directive
		{
			name:    "one backslash ok not directive",
			report:  "1..1\nok 1 - desc \\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 0,
		},
		{
			name:    "one backslash not ok not directive",
			report:  "1..2\nok 1 - pass\nnot ok 2 - desc \\# SKIP reason\n",
			outcome: "failed",
			passed:  1,
			failed:  1,
			skipped: 0,
		},
		// 2 backslashes before #: even -> unescaped directive (SKIP)
		{
			name:    "two backslashes ok skip",
			report:  "1..2\nok 1 - pass\nok 2 - desc \\\\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "two backslashes not ok skip",
			report:  "1..2\nok 1 - pass\nnot ok 2 - desc \\\\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		// 3 backslashes before #: odd -> escaped hash, not a directive
		{
			name:    "three backslashes ok not directive",
			report:  "1..1\nok 1 - desc \\\\\\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 0,
		},
		{
			name:    "three backslashes not ok not directive",
			report:  "1..2\nok 1 - pass\nnot ok 2 - desc \\\\\\# SKIP reason\n",
			outcome: "failed",
			passed:  1,
			failed:  1,
			skipped: 0,
		},
		// 4 backslashes before #: even -> unescaped directive (SKIP)
		{
			name:    "four backslashes ok skip",
			report:  "1..2\nok 1 - pass\nok 2 - desc \\\\\\\\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "four backslashes not ok skip",
			report:  "1..2\nok 1 - pass\nnot ok 2 - desc \\\\\\\\# SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		// Escaped hash followed by real unescaped directive
		{
			name:    "escaped hash followed by real skip ok",
			report:  "1..2\nok 1 - pass\nok 2 - test \\#1 # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "escaped hash followed by real skip not ok",
			report:  "1..2\nok 1 - pass\nnot ok 2 - test \\#1 # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "three backslashes followed by real skip",
			report:  "1..2\nok 1 - pass\nnot ok 2 - test \\\\\\#1 # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		{
			name:    "escaped hash followed by unsupported todo",
			report:  "1..2\nok 1 - pass\nnot ok 2 - test \\#1 # TODO fix\n",
			outcome: "unknown",
			passed:  1,
			failed:  0,
			skipped: 0,
		},
		{
			name:    "escaped skip followed by real todo",
			report:  "1..2\nok 1 - pass\nnot ok 2 - test \\# SKIP # TODO fix\n",
			outcome: "unknown",
			passed:  1,
			failed:  0,
			skipped: 0,
		},
		{
			name:    "escaped todo followed by real skip",
			report:  "1..2\nok 1 - pass\nnot ok 2 - test \\# TODO # SKIP reason\n",
			outcome: "passed",
			passed:  1,
			failed:  0,
			skipped: 1,
		},
		// TODO stays unknown when real directive
		{
			name:    "real todo directive stays unknown",
			report:  "1..1\nnot ok 1 - test # TODO later\n",
			outcome: "unknown",
			passed:  0,
			failed:  0,
			skipped: 0,
		},
		// Escaped hash with TODO word is literal description, not directive
		{
			name:    "literal escaped todo failure",
			report:  "1..1\nnot ok 1 - test \\# TODO failure\n",
			outcome: "failed",
			passed:  0,
			failed:  1,
			skipped: 0,
		},
		// Incomplete stays unknown
		{
			name:    "incomplete with escaped hash failure",
			report:  "1..2\nnot ok 1 - literal \\# SKIP failure\n",
			outcome: "unknown",
			passed:  0,
			failed:  1,
			skipped: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseFlatTAP([]byte(tc.report))
			if out.VerificationOutcome != tc.outcome || out.Passed != tc.passed || out.Failed != tc.failed || out.Skipped != tc.skipped || out.Complete != (tc.outcome != "unknown") {
				t.Fatalf("unexpected TAP verdict for %s: %+v", tc.name, out)
			}
		})
	}
}

func TestVerificationFlatTAPDirectiveEscapingFailureBounds(t *testing.T) {
	var report strings.Builder
	report.WriteString("1..101\n")
	for i := 1; i <= 101; i++ {
		fmt.Fprintf(&report, "not ok %d - item \\#%d %s\n", i, i, strings.Repeat("界", 1000))
	}
	out := parseFlatTAP([]byte(report.String()))
	if out.VerificationOutcome != "failed" || out.Failed != 101 || len(out.Failures) != 100 || !out.FailuresTruncated {
		t.Fatalf("failure limits: %+v", out)
	}
	for _, failure := range out.Failures {
		if len(failure.Test) > 2048 || !utf8.ValidString(failure.Test) || failure.Kind != "test_failure" {
			t.Fatalf("invalid failure: %+v", failure)
		}
		if !strings.HasPrefix(failure.Test, "item \\#") {
			t.Fatalf("expected failure test prefix 'item \\#', got %q", failure.Test)
		}
	}
}
