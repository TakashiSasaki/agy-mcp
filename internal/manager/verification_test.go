package manager

import "testing"

// Mutation guards: complete-report/counter checks in parseJUnit and explicit
// exception-type matching. A narrative timeout never becomes a product verdict.
func TestVerificationReportVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, xml, outcome    string
		tests, failed, errors int
	}{
		{"pass", `<testsuite tests="1" failures="0"><testcase name="ok"/></testsuite>`, "passed", 1, 0, 0},
		{"nested pass", `<testsuites tests="2"><testsuite tests="1"><testcase name="a"/></testsuite><testsuite tests="1"><testcase name="b"/></testsuite></testsuites>`, "passed", 2, 0, 0},
		{"failure", `<testsuite tests="1" failures="1"><testcase name="bad"><failure type="AssertionError" message="mismatch">details</failure></testcase></testsuite>`, "failed", 1, 1, 0},
		{"harness timeout", `<testsuite tests="1" errors="1"><testcase name="receive"><error type="subprocess.TimeoutExpired" message="120 second harness deadline">receiver may still run</error></testcase></testsuite>`, "failed", 1, 0, 1},
		{"counter mismatch", `<testsuite tests="2"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"negative count", `<testsuite tests="-1"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"truncated", `<testsuite tests="1"><testcase name="one"/>`, "unknown", 1, 0, 0},
		{"empty", `<testsuite tests="0"/>`, "unknown", 0, 0, 0},
		{"skipped only", `<testsuite tests="1" skipped="1"><testcase name="one"><skipped/></testcase></testsuite>`, "unknown", 1, 0, 0},
		{"unrelated XML", `<log><testcase name="one"/></log>`, "unknown", 0, 0, 0},
		{"embedded XML in output", `<testsuite tests="1"><system-out><testcase name="not-a-case"/></system-out></testsuite>`, "unknown", 0, 0, 0},
		{"directive", `<!DOCTYPE testsuite><testsuite tests="1"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseJUnit([]byte(tc.xml))
			if out.VerificationOutcome != tc.outcome || out.Tests != tc.tests || out.Failed != tc.failed || out.Errors != tc.errors {
				t.Fatalf("report classification: %+v", out)
			}
			if tc.outcome == "unknown" && out.Complete {
				t.Fatal("unknown/incomplete evidence presented as complete")
			}
			if tc.name == "harness timeout" && (len(out.Failures) != 1 || out.Failures[0].Kind != "subprocess_timeout") {
				t.Fatalf("explicit harness timeout lost: %+v", out)
			}
		})
	}
}
