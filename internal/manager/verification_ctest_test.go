package manager

import (
	"fmt"
	"slices"
	"testing"
)

// CTest 3.28.3 emitted status="notrun" and a skipped element for both
// intentional skips and infrastructure failures. Preserve that distinction.
func TestVerificationCTestSkipReasons(t *testing.T) {
	for _, tc := range []struct {
		name, attrs, reason, disabled, outcome, warning string
		passed, skipped                                 int
	}{
		{"skip return code", `status="notrun"`, "SKIP_RETURN_CODE=77", "0", "passed", "", 1, 1},
		{"skip regex", `status="notrun"`, "SKIP_REGULAR_EXPRESSION_MATCHED", "0", "passed", "", 1, 1},
		{"missing executable", `status="notrun"`, "Unable to find executable", "0", "unknown", "unsupported_notrun_testcase", 1, 1},
		{"failed working directory", `status="notrun"`, "Failed to change working directory", "0", "unknown", "unsupported_notrun_testcase", 1, 1},
		{"failed fixture", `status="notrun"`, "Fixture dependency failed", "0", "unknown", "unsupported_notrun_testcase", 1, 1},
		{"out of range return code", `status="notrun"`, "SKIP_RETURN_CODE=256", "0", "unknown", "unsupported_notrun_testcase", 1, 1},
		{"disabled testcase", `status="disabled"`, "", "1", "unknown", "unsupported_disabled_tests", 1, 0},
		{"GoogleTest suppression wins", `status="notrun" result="suppressed"`, "SKIP_RETURN_CODE=77", "0", "unknown", "unsupported_disabled_tests", 1, 1},
		{"generic skip unchanged", ``, "for any reason", "0", "passed", "", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marker string
			if tc.reason != "" {
				marker = fmt.Sprintf(`<skipped message=%q/>`, tc.reason)
			}
			report := fmt.Appendf(nil, `<testsuite tests="2" failures="0" disabled=%q skipped="%d"><testcase name="ok" status="run"/><testcase name="probe" %s>%s</testcase></testsuite>`, tc.disabled, tc.skipped, tc.attrs, marker)
			got := parseJUnit(report)
			if got.VerificationOutcome != tc.outcome || got.Passed != tc.passed || got.Skipped != tc.skipped || got.Complete != (tc.outcome == "passed") || (tc.warning != "" && !slices.Contains(got.Warnings, tc.warning)) {
				t.Fatalf("CTest report verdict: %+v", got)
			}
		})
	}
}

// Mutation guard: encoding/xml accepts contradictory duplicate attributes.
func TestVerificationCTestContradictorySkipReasons(t *testing.T) {
	for _, marker := range []string{
		`<skipped message="SKIP_RETURN_CODE=77" message="Unable to find executable"/>`,
		`<skipped message="Unable to find executable" message="SKIP_RETURN_CODE=77"/>`,
		`<skipped message="SKIP_RETURN_CODE=77"/><skipped message="SKIP_RETURN_CODE=77"/>`,
	} {
		report := fmt.Appendf(nil, `<testsuite tests="2" failures="0" skipped="1"><testcase name="ok"/><testcase name="probe" status="notrun">%s</testcase></testsuite>`, marker)
		got := parseJUnit(report)
		if got.Complete || got.VerificationOutcome != "unknown" || !slices.Contains(got.Warnings, "unsupported_notrun_testcase") {
			t.Fatalf("contradictory CTest skip evidence: %+v", got)
		}
	}
}
