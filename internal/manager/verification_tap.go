package manager

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	tapPlanPattern = regexp.MustCompile(`^1\.\.(\d+)$`)
	tapCasePattern = regexp.MustCompile(`^(ok|not ok)[ \t]+([1-9]\d*)(?:[ \t]+-(?:[ \t]+(.*))?)?$`)
)

// parseFlatTAP accepts a deliberately restricted, flat TAP report. The shape
// was measured from the C byte histogram shell harness on 2026-11-07. Nested
// subtests, YAML diagnostics and TODO directives are unsupported, not ignored.
func parseFlatTAP(data []byte) VerificationResult {
	out := VerificationResult{Source: "tap_artifact", VerificationOutcome: evidenceOutcomeUnknown,
		Failures: []VerificationFailure{}, Warnings: []string{}}
	warn := func(code string) {
		if !slices.Contains(out.Warnings, code) {
			out.Warnings = append(out.Warnings, code)
		}
	}
	if !utf8.Valid(data) {
		warn("invalid_tap_utf8")
	}
	plan, seenPlan, trailingPlan, seenContent := 0, false, false, false
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line == "TAP version 13" && !seenContent {
			seenContent = true
			continue
		}
		seenContent = true
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "# Subtest:") {
				warn("unsupported_tap_subtest")
			}
			continue
		}
		if strings.HasPrefix(line, "Bail out!") {
			warn("tap_bailout")
			continue
		}
		if match := tapPlanPattern.FindStringSubmatch(line); match != nil {
			value, err := strconv.Atoi(match[1])
			if seenPlan || err != nil {
				warn("invalid_tap_plan")
			}
			plan, seenPlan, trailingPlan = value, true, out.Tests > 0
			continue
		}
		skipped := false
		if before, directive, found := strings.Cut(line, "#"); found {
			fields := strings.Fields(directive)
			if len(fields) == 0 || !strings.EqualFold(fields[0], "SKIP") {
				warn("unsupported_tap_directive")
				continue
			}
			skipped, line = true, strings.TrimRight(before, " \t")
		}
		match := tapCasePattern.FindStringSubmatch(line)
		if match == nil {
			warn("unsupported_or_malformed_tap_line")
			continue
		}
		out.Tests++
		number, err := strconv.Atoi(match[2])
		if err != nil || number != out.Tests || trailingPlan {
			warn("invalid_tap_sequence")
		}
		switch {
		case skipped:
			out.Skipped++
		case match[1] == "not ok":
			out.Failed++
			if len(out.Failures) < 100 {
				name := match[3]
				if name == "" {
					name = "test " + match[2]
				}
				out.Failures = append(out.Failures, VerificationFailure{
					Test: truncateUTF8(name, 2048), Kind: "test_failure", Message: "TAP not ok"})
			} else {
				out.FailuresTruncated = true
			}
		default:
			out.Passed++
		}
	}
	if !seenPlan || plan != out.Tests {
		warn("missing_or_mismatched_tap_plan")
	}
	if out.Tests == 0 || out.Tests == out.Skipped {
		warn("no_executed_testcases")
	}
	return finishJUnit(out, junitCounts{tests: out.Tests, passed: out.Passed,
		failed: out.Failed, skipped: out.Skipped}, len(out.Warnings) == 0)
}
