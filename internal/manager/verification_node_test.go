package manager

import (
	"slices"
	"strings"
	"testing"
)

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

// MEASURED against Node.js v24.22.2 with a parent that awaits a successful
// child and then throws an assertion error. Node exits 1, but only its footer
// represents the failed parent. Removing validateNodeSummary must fail this test.
func TestVerificationNodeParentFailure(t *testing.T) {
	report := `<testsuites><testsuite name="parent" tests="1" failures="0" errors="0" skipped="0"><testcase name="child pass"/></testsuite>
<!-- tests 2 --><!-- suites 0 --><!-- pass 1 --><!-- fail 1 -->
<!-- cancelled 0 --><!-- skipped 0 --><!-- todo 0 --></testsuites>`
	out := parseJUnit([]byte(report))
	if out.VerificationOutcome != "unknown" || out.Complete || !slices.Contains(out.Warnings, "unrepresented_node_failure") {
		t.Fatalf("missing parent failure must not pass: %+v", out)
	}
}

func TestVerificationNodeFooter(t *testing.T) {
	footer := `<!-- tests 1 --><!-- suites 0 --><!-- pass 1 --><!-- fail 0 --><!-- cancelled 0 --><!-- skipped 0 --><!-- todo 0 -->`
	for _, tc := range []struct {
		name, cases, footer, outcome string
	}{
		{"ordinary pass", `<testcase name="ok"/>`, footer, "passed"},
		{"generic JUnit", `<testcase name="ok"/>`, "", "passed"},
		{"duplicate", `<testcase name="ok"/>`, footer + `<!-- fail 0 -->`, "unknown"},
		{"contradictory duplicate", `<testcase name="ok"/>`, footer + `<!-- fail 1 -->`, "unknown"},
		{"negative", `<testcase name="ok"/>`, strings.Replace(footer, "fail 0", "fail -1", 1), "unknown"},
		{"malformed", `<testcase name="ok"/>`, strings.Replace(footer, "fail 0", "fail nope", 1), "unknown"},
		{"overflow", `<testcase name="ok"/>`, strings.Replace(footer, "fail 0", "fail 99999999999999999999999999", 1), "unknown"},
		{"missing", `<testcase name="ok"/>`, strings.Replace(footer, "<!-- fail 0 -->", "", 1), "unknown"},
		{"sum mismatch", `<testcase name="ok"/>`, strings.Replace(footer, "tests 1", "tests 2", 1), "unknown"},
		{"too few total", `<testcase name="ok"/><testcase name="another"/>`, footer, "unknown"},
		{"unrepresented cancelled", `<testcase name="ok"/>`, strings.Replace(strings.Replace(footer, "tests 1", "tests 2", 1), "cancelled 0", "cancelled 1", 1), "unknown"},
		{"parent pass adds total", `<testsuite tests="1"><testcase name="child"/></testsuite>`, strings.Replace(strings.Replace(footer, "tests 1", "tests 2", 1), "pass 1", "pass 2", 1), "passed"},
		{"skip and todo", `<testcase name="ok"/><testcase name="skip"><skipped/></testcase><testcase name="todo"><skipped type="todo"/></testcase>`, `<!-- tests 3 --><!-- suites 0 --><!-- pass 1 --><!-- fail 0 --><!-- cancelled 0 --><!-- skipped 1 --><!-- todo 1 -->`, "passed"},
		{"failing todo stays conservative", `<testcase name="todo"><skipped type="todo"/><failure>expected failure</failure></testcase>`, `<!-- tests 1 --><!-- suites 0 --><!-- pass 0 --><!-- fail 0 --><!-- cancelled 0 --><!-- skipped 0 --><!-- todo 1 -->`, "failed"},
		{"explicit failure", `<testcase name="bad"><failure>assertion</failure></testcase>`, strings.Replace(strings.Replace(footer, "pass 1", "pass 0", 1), "fail 0", "fail 1", 1), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseJUnit([]byte(`<testsuites>` + tc.cases + tc.footer + `</testsuites>`))
			if out.VerificationOutcome != tc.outcome || out.Complete != (tc.outcome != "unknown") {
				t.Fatalf("footer verdict: %+v", out)
			}
		})
	}
}
