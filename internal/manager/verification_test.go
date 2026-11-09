package manager

import (
	"slices"
	"testing"
)

// Mutation guards: complete-report/counter checks in parseJUnit and explicit
// exception-type matching. A narrative timeout never becomes a product verdict.
func TestVerificationReportVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, xml, outcome    string
		tests, failed, errors int
	}{
		{"pass", `<testsuite tests="1" failures="0"><testcase name="ok"/></testsuite>`, "passed", 1, 0, 0},
		{"nested pass", `<testsuites tests="2"><testsuite tests="1"><testcase name="a"/></testsuite><testsuite tests="1"><testcase name="b"/></testsuite></testsuites>`, "passed", 2, 0, 0},
		{"nested suites without counters", `<testsuite><testsuite><testcase name="a"/></testsuite></testsuite>`, "passed", 1, 0, 0},
		{"failure", `<testsuite tests="1" failures="1"><testcase name="bad"><failure type="AssertionError" message="mismatch">details</failure></testcase></testsuite>`, "failed", 1, 1, 0},
		{"harness timeout", `<testsuite tests="1" errors="1"><testcase name="receive"><error type="subprocess.TimeoutExpired" message="120 second harness deadline">receiver may still run</error></testcase></testsuite>`, "failed", 1, 0, 1},
		{"counter mismatch", `<testsuite tests="2"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"negative count", `<testsuite tests="-1"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"empty count", `<testsuite tests=""><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"duplicate count", `<testsuite tests="1" tests="2"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"duplicate identical count", `<testsuite tests="1" tests="1"><testcase name="one"/></testsuite>`, "unknown", 1, 0, 0},
		{"suite inside testcase", `<testsuite tests="1"><testcase name="one"><testsuite tests="0"/></testcase></testsuite>`, "unknown", 1, 0, 0},
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

// Mutation guards: markSkipped DecodeElement consumption, failure Nested check,
// and skipping invalid nested elements inside an open testcase preserve outer
// testcase closure and prevent nested elements inside skipped/failure from passing.
func TestVerificationNestedResultAndTestcaseDepth(t *testing.T) {
	for _, tc := range []struct {
		name, xml, warning               string
		tests, passed, failed, skipped   int
		wantFailures                     int
	}{
		{
			name:    "element nested inside skipped",
			xml:     `<testsuite tests="2" skipped="1"><testcase name="ok"/><testcase name="skip"><skipped><system-out>nested</system-out></skipped></testcase></testsuite>`,
			warning: "invalid_report_element",
			tests:   2, passed: 1, failed: 0, skipped: 1, wantFailures: 0,
		},
		{
			name:    "testcase nested inside skipped before outer failure",
			xml:     `<testsuite tests="1" failures="1"><testcase name="outer"><skipped><testcase name="inner"/></skipped><failure message="boom">outer failed</failure></testcase></testsuite>`,
			warning: "invalid_report_element",
			tests:   1, passed: 0, failed: 1, skipped: 0, wantFailures: 1,
		},
		{
			name:    "testcase nested inside testcase before outer failure",
			xml:     `<testsuite tests="1" failures="1"><testcase name="outer"><testcase name="inner"/><failure message="boom">outer failed</failure></testcase></testsuite>`,
			warning: "invalid_testcase_nesting",
			tests:   1, passed: 0, failed: 1, skipped: 0, wantFailures: 1,
		},
		{
			name:    "suite with testcase nested inside testcase before outer failure",
			xml:     `<testsuite tests="1" failures="1"><testcase name="outer"><testsuite><testcase name="inner"/></testsuite><failure message="boom">outer failed</failure></testcase></testsuite>`,
			warning: "invalid_suite_nesting",
			tests:   1, passed: 0, failed: 1, skipped: 0, wantFailures: 1,
		},
		{
			name:    "element nested inside failure",
			xml:     `<testsuite tests="1" failures="1"><testcase name="bad"><failure message="boom"><system-out>nested</system-out></failure></testcase></testsuite>`,
			warning: "invalid_failure_detail",
			tests:   1, passed: 0, failed: 1, skipped: 0, wantFailures: 1,
		},
		{
			name:    "orphan failure containing testcase",
			xml:     `<testsuite tests="1" failures="0"><failure><testcase name="fake"/></failure><testcase name="ok"/></testsuite>`,
			warning: "orphan_test_result",
			tests:   1, passed: 1, failed: 0, skipped: 0, wantFailures: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseJUnit([]byte(tc.xml))
			if out.Complete || out.VerificationOutcome != "unknown" || out.Tests != tc.tests || out.Passed != tc.passed || out.Failed != tc.failed || out.Skipped != tc.skipped || len(out.Failures) != tc.wantFailures || !slices.Contains(out.Warnings, tc.warning) {
				t.Fatalf("nested structure handling mismatch: %+v", out)
			}
		})
	}
}

