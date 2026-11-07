package manager

import "testing"

// MEASURED against Node.js v24.22.2 with node --test --test-reporter=junit.
// Timings and the absolute source path are omitted; the native nesting and
// summary comments are preserved. No Node installation is needed by this test.
const nodeJUnitReport = `<?xml version="1.0" encoding="utf-8"?>
<testsuites>
  <testcase name="native Node pass" classname="test"/>
  <testcase name="native Node skipped" classname="test">
    <skipped type="skipped" message="true"/>
  </testcase>
  <!-- tests 2 -->
  <!-- suites 0 -->
  <!-- pass 1 -->
  <!-- fail 0 -->
  <!-- cancelled 0 -->
  <!-- skipped 1 -->
  <!-- todo 0 -->
</testsuites>`

// Mutation guard: reverting rootCases acceptance in junitParser.start must make
// the native report unknown. Structural and counter checks must still reject
// incomplete or inconsistent evidence in this producer shape.
func TestVerificationNodeJUnit(t *testing.T) {
	for _, tc := range []struct {
		name, report, outcome           string
		passed, failed, errors, skipped int
	}{
		{"native pass and skip", nodeJUnitReport, "passed", 1, 0, 0, 1},
		{"native failure", `<testsuites><testcase name="bad"><failure type="testCodeFailure">assertion failed</failure></testcase></testsuites>`, "failed", 0, 1, 0, 0},
		{"native error", `<testsuites><testcase name="bad"><error type="Error">setup failed</error></testcase></testsuites>`, "failed", 0, 0, 1, 0},
		{"mixed root and suite", `<testsuites tests="2"><testcase name="root"/><testsuite tests="1"><testcase name="nested"/></testsuite></testsuites>`, "passed", 2, 0, 0, 0},
		{"truncated native", `<testsuites><testcase name="ok"/>`, "unknown", 1, 0, 0, 0},
		{"native counters disagree", `<testsuites tests="2"><testcase name="ok"/></testsuites>`, "unknown", 1, 0, 0, 0},
		{"native skipped only", `<testsuites><testcase name="skip"><skipped/></testcase></testsuites>`, "unknown", 0, 0, 0, 1},
		{"nested testcase", `<testsuites><testcase name="outer"><testcase name="inner"/></testcase></testsuites>`, "unknown", 1, 0, 0, 0},
		{"suite in testcase", `<testsuites><testcase name="outer"><testsuite/></testcase></testsuites>`, "unknown", 1, 0, 0, 0},
		{"case inside output ignored", `<testsuites><system-out><testcase name="fake"/></system-out></testsuites>`, "unknown", 0, 0, 0, 0},
		{"unmeasured nested testsuites", `<testsuites><testsuites><testcase name="nested"/></testsuites></testsuites>`, "unknown", 1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseJUnit([]byte(tc.report))
			if out.VerificationOutcome != tc.outcome || out.Passed != tc.passed || out.Failed != tc.failed || out.Errors != tc.errors || out.Skipped != tc.skipped {
				t.Fatalf("Node report verdict: %+v", out)
			}
			if out.Complete != (tc.outcome != "unknown") {
				t.Fatalf("invalid completeness: %+v", out)
			}
		})
	}
}
