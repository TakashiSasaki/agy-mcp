package manager

import (
	"fmt"
	"slices"
	"testing"
)

// Mutation guard: inspecting only the first execution attribute can turn a
// contradictory duplicate into a pass because encoding/xml accepts duplicates.
func TestVerificationDuplicateExecutionAttributes(t *testing.T) {
	for _, attrs := range []string{
		`status="run" status="notrun"`, `status="notrun" status="run"`,
		`result="completed" result="suppressed"`, `result="suppressed" result="completed"`,
		`status="run" status="run"`, `result="completed" result="completed"`,
	} {
		out := parseJUnit(fmt.Appendf(nil, `<testsuite tests="1"><testcase %s/></testsuite>`, attrs))
		if out.Complete || out.VerificationOutcome != "unknown" {
			t.Fatalf("duplicate execution attributes passed: %s %+v", attrs, out)
		}
	}
}

// MEASURED against GoogleTest v1.16.0 (commit 6910c9d9165801d8827d628cb72eb7ea9dd538c5)
// with DISABLED_ testcase in disabled_test.cc. 0 tests ran, command exited 0.
// Native fixture native-disabled.xml:
const nativeGoogleTestDisabledXML = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="0" disabled="1" errors="0" time="0." timestamp="2026-11-07T07:39:34.454" name="AllTests">
  <testsuite name="Disabled" tests="1" failures="0" disabled="1" skipped="0" errors="0" time="0." timestamp="1970-01-01T00:00:00.000">
    <testcase name="DISABLED_NeverRan" file="disabled_test.cc" line="2" status="notrun" result="suppressed" time="0." timestamp="1970-01-01T00:00:00.000" classname="Disabled" />
  </testsuite>
</testsuites>`

const compactGoogleTestDisabledXML = `<testsuites tests="1" failures="0" disabled="1" errors="0"><testsuite name="Disabled" tests="1" failures="0" disabled="1" errors="0"><testcase name="DISABLED_NeverRan" status="notrun" result="suppressed" classname="Disabled" time="0"/></testsuite></testsuites>`

func TestVerificationDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, report, outcome                  string
		tests, passed, failed, errors, skipped int
	}{
		{
			name:    "native googletest disabled report",
			report:  nativeGoogleTestDisabledXML,
			outcome: "unknown",
			tests:   1, passed: 0, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "compact googletest disabled report",
			report:  compactGoogleTestDisabledXML,
			outcome: "unknown",
			tests:   1, passed: 0, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "testcase status notrun and result suppressed",
			report:  `<testsuite tests="1" failures="0"><testcase name="dis" status="notrun" result="suppressed"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 0, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "testcase status notrun only",
			report:  `<testsuite tests="1" failures="0"><testcase name="dis" status="notrun"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 0, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "testcase result suppressed only",
			report:  `<testsuite tests="1" failures="0"><testcase name="dis" result="suppressed"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 0, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "mixed executed and disabled with suite counter",
			report:  `<testsuite tests="2" failures="0" disabled="1"><testcase name="ok" status="run" result="completed"/><testcase name="dis" status="notrun" result="suppressed"/></testsuite>`,
			outcome: "unknown",
			tests:   2, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "mixed executed and disabled without suite counter",
			report:  `<testsuite tests="2" failures="0"><testcase name="ok"/><testcase name="dis" status="notrun" result="suppressed"/></testsuite>`,
			outcome: "unknown",
			tests:   2, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "suite positive disabled counter with ordinary testcase",
			report:  `<testsuite tests="1" failures="0" disabled="1"><testcase name="ok"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "suite malformed disabled counter",
			report:  `<testsuite tests="1" failures="0" disabled="bogus"><testcase name="ok"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "suite negative disabled counter",
			report:  `<testsuite tests="1" failures="0" disabled="-1"><testcase name="ok"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "suite empty disabled counter",
			report:  `<testsuite tests="1" failures="0" disabled=""><testcase name="ok"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "suite duplicate disabled counter",
			report:  `<testsuite tests="1" failures="0" disabled="0" disabled="0"><testcase name="ok"/></testsuite>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "testsuites positive disabled counter",
			report:  `<testsuites tests="1" failures="0" disabled="1"><testsuite tests="1" failures="0"><testcase name="ok"/></testsuite></testsuites>`,
			outcome: "unknown",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "normal googletest run completed disabled 0 passes",
			report:  `<testsuites tests="1" failures="0" disabled="0" errors="0"><testsuite name="Normal" tests="1" failures="0" disabled="0" errors="0"><testcase name="ok" status="run" result="completed"/></testsuite></testsuites>`,
			outcome: "passed",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
		{
			name:    "ordinary junit without disabled attrs unchanged",
			report:  `<testsuite tests="1" failures="0"><testcase name="ok"/></testsuite>`,
			outcome: "passed",
			tests:   1, passed: 1, failed: 0, errors: 0, skipped: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := parseJUnit([]byte(tc.report))
			if out.VerificationOutcome != tc.outcome || out.Tests != tc.tests || out.Passed != tc.passed || out.Failed != tc.failed || out.Errors != tc.errors || out.Skipped != tc.skipped {
				t.Fatalf("disabled report verdict mismatch: outcome=%s (want %s), tests=%d (want %d), passed=%d (want %d), failed=%d (want %d), errors=%d (want %d), skipped=%d (want %d), warnings=%v",
					out.VerificationOutcome, tc.outcome, out.Tests, tc.tests, out.Passed, tc.passed, out.Failed, tc.failed, out.Errors, tc.errors, out.Skipped, tc.skipped, out.Warnings)
			}
			if tc.outcome == "unknown" && out.Complete {
				t.Fatal("unknown/incomplete disabled evidence presented as complete")
			}
			if tc.outcome != "unknown" && !out.Complete {
				t.Fatal("valid report presented as incomplete")
			}
			if tc.name == "native googletest disabled report" {
				if !slices.Contains(out.Warnings, "no_executed_testcases") {
					t.Fatalf("native disabled report must warn no_executed_testcases: warnings=%v", out.Warnings)
				}
			}
		})
	}
}

// Mutation guard: removing the default unsupported_testcase_execution_attribute
// branch in testcaseExecution or the unsupported case in end() turns unknown
// status/result attributes into passing testcases.
func TestVerificationUnknownExecutionAttributes(t *testing.T) {
	for _, report := range []string{
		`<testsuite tests="1" failures="0"><testcase name="bad" status="fail"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" status="failed"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" status="error"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" status="skipped"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" status=""/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" result="failed"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" result="skipped"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" result="cancelled"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" result=""/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="bad" status="run" result="skipped"/></testsuite>`,
		`<testsuite xmlns:foreign="urn:diagnostics" tests="1" failures="0"><testcase name="bad" foreign:status="run"/></testsuite>`,
		`<testsuite xmlns:foreign="urn:diagnostics" tests="1" failures="0"><testcase name="bad" foreign:result="completed"/></testsuite>`,
	} {
		out := parseJUnit([]byte(report))
		if out.Complete || out.VerificationOutcome != "unknown" || out.Tests != 1 || out.Passed != 0 || !slices.Contains(out.Warnings, "unsupported_testcase_execution_attribute") {
			t.Fatalf("unsupported execution attribute accepted: %s -> %+v", report, out)
		}
	}
	for _, report := range []string{
		`<testsuite tests="1" failures="0"><testcase name="ok" status="run"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="ok" result="completed"/></testsuite>`,
		`<testsuite tests="1" failures="0"><testcase name="ok" status="run" result="completed"/></testsuite>`,
	} {
		out := parseJUnit([]byte(report))
		if !out.Complete || out.VerificationOutcome != "passed" || out.Tests != 1 || out.Passed != 1 || len(out.Warnings) != 0 {
			t.Fatalf("valid execution attribute rejected: %s -> %+v", report, out)
		}
	}
}

