package manager

import (
	"fmt"
	"testing"
)

func TestCTestReviewRejectsForeignAndContradictorySkipEvidence(t *testing.T) {
	for _, tc := range []struct{ name, root, element, attributes, marker string }{
		{"foreign root", "foreign:testsuite", "testcase", `status="notrun"`, `<skipped message="SKIP_RETURN_CODE=77"/>`},
		{"foreign testcase", "testsuite", "foreign:testcase", `status="notrun"`, `<skipped message="SKIP_RETURN_CODE=77"/>`},
		{"foreign element", "testsuite", "testcase", `status="notrun"`, `<foreign:skipped message="SKIP_RETURN_CODE=77"/>`},
		{"foreign message attribute", "testsuite", "testcase", `status="notrun"`, `<skipped foreign:message="SKIP_RETURN_CODE=77"/>`},
		{"explicit result contradicts notrun", "testsuite", "testcase", `status="notrun" result="completed"`, `<skipped message="SKIP_RETURN_CODE=77"/>`},
		{"foreign testcase status", "testsuite", "testcase", `foreign:status="notrun"`, `<skipped message="SKIP_RETURN_CODE=77"/>`},
		{"nested skip in diagnostic output", "testsuite", "testcase", `status="notrun"`, `<system-out><skipped message="SKIP_RETURN_CODE=77"/></system-out>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := fmt.Appendf(nil, `<%s xmlns:foreign="urn:diagnostics" tests="2" failures="0" skipped="1"><testcase name="ok" status="run"/><%s name="probe" %s>%s</%s></%s>`, tc.root, tc.element, tc.attributes, tc.marker, tc.element, tc.root)
			got := parseJUnit(report)
			if got.VerificationOutcome != "unknown" {
				t.Fatalf("notrun with unproven CTest skip treated as %q: %+v", got.VerificationOutcome, got)
			}
		})
	}
}
