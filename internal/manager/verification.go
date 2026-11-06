package manager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

const (
	MaxVerificationBytes = 4 << 20
	junitSuiteTag        = "testsuite"
	junitSuitesTag       = "testsuites"
	junitSkippedTag      = "skipped"
)

type VerificationFailure struct {
	Test         string `json:"test" jsonschema:"testcase name reported in the XML"`
	Class        string `json:"class,omitempty" jsonschema:"testcase classname reported in the XML"`
	Kind         string `json:"kind" jsonschema:"test_failure, test_error or subprocess_timeout; the latter requires an explicit TimeoutExpired exception type"`
	ReportedType string `json:"reported_type,omitempty" jsonschema:"failure/error type from XML, not a model diagnosis of the product"`
	Message      string `json:"message,omitempty" jsonschema:"reported message, bounded to 2048 UTF-8 bytes"`
	Detail       string `json:"detail,omitempty" jsonschema:"reported failure text, bounded to 2048 UTF-8 bytes; full details remain in the artifact"`
}

type VerificationResult struct {
	JobID               string                `json:"job_id" jsonschema:"job whose associated artifact was inspected"`
	JobState            string                `json:"job_state" jsonschema:"independent job lifecycle state; does not set the verification outcome"`
	Source              string                `json:"source" jsonschema:"junit_xml_artifact; this is a parsed report, not independently witnessed command execution"`
	Scope               string                `json:"scope" jsonschema:"declared or legacy_workspace artifact read scope"`
	Path                string                `json:"path" jsonschema:"requested report path within job-associated roots"`
	SHA256              string                `json:"sha256" jsonschema:"SHA-256 of the exact report bytes parsed"`
	SizeBytes           int                   `json:"size_bytes" jsonschema:"size of the report parsed, bounded to 4 MiB"`
	VerificationOutcome string                `json:"verification_outcome" jsonschema:"passed, failed or unknown according to the complete report; never inferred from job_state"`
	Complete            bool                  `json:"complete" jsonschema:"XML is complete and declared counters agree with observed testcases; empty or incomplete evidence cannot pass"`
	Tests               int                   `json:"tests" jsonschema:"testcases observed in the parsed report"`
	Passed              int                   `json:"passed" jsonschema:"observed testcases without failure, error or skipped markers"`
	Failed              int                   `json:"failed" jsonschema:"observed testcases with failure markers"`
	Errors              int                   `json:"errors" jsonschema:"observed testcases with error markers"`
	Skipped             int                   `json:"skipped" jsonschema:"observed skipped testcases"`
	Failures            []VerificationFailure `json:"failures" jsonschema:"up to 100 reported failure/error details"`
	FailuresTruncated   bool                  `json:"failures_truncated" jsonschema:"more details exist in the report than were included here"`
	Warnings            []string              `json:"warnings" jsonschema:"missing, malformed or inconsistent evidence; prevents a pass verdict"`
}

type junitCounts struct{ tests, passed, failed, errors, skipped int }
type junitSuite struct {
	before     junitCounts
	attributes []xml.Attr
	depth      int
	name       string
}
type junitCase struct {
	name, class              string
	failed, errored, skipped bool
}

func (m *Manager) Verification(jobID, path string) (VerificationResult, error) {
	meta, dir, roots, err := m.resolveJobAndRoots(jobID)
	if err != nil {
		return VerificationResult{}, err
	}
	target, err := artifactTarget(meta, dir, path)
	if err != nil {
		return VerificationResult{}, err
	}
	f, err := openArtifactFile(roots, target)
	if err != nil {
		return VerificationResult{}, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return VerificationResult{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxVerificationBytes+1))
	if err != nil {
		return VerificationResult{}, err
	}
	if len(data) > MaxVerificationBytes {
		return VerificationResult{}, fmt.Errorf("verification report exceeds 4 MiB")
	}
	after, err := f.Stat()
	if err != nil {
		return VerificationResult{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return VerificationResult{}, fmt.Errorf("verification report changed while reading; retry after the producer closes it")
	}
	st, err := m.Status(jobID)
	if err != nil {
		return VerificationResult{}, err
	}
	out := parseJUnit(data)
	out.JobID, out.JobState, out.Scope, out.Path = jobID, st.State, artifactScope(meta), path
	digest := sha256.Sum256(data)
	out.SHA256 = hex.EncodeToString(digest[:])
	out.SizeBytes = len(data)
	return out, nil
}

func xmlAttribute(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func validateJUnitCounters(suite junitSuite, counts junitCounts) bool {
	values := map[string]int{"tests": counts.tests - suite.before.tests, "failures": counts.failed - suite.before.failed, "errors": counts.errors - suite.before.errors, junitSkippedTag: counts.skipped - suite.before.skipped}
	seen := make(map[string]bool)
	for _, attr := range suite.attributes {
		name := attr.Name.Local
		want, known := values[name]
		if !known {
			continue
		}
		got, err := strconv.Atoi(attr.Value)
		if seen[name] || err != nil || got < 0 || got != want {
			return false
		}
		seen[name] = true
	}
	return true
}

// Parsing reports classifies evidence, not the product or the agent's narrative.
type junitParser struct {
	decoder                     *xml.Decoder
	out                         VerificationResult
	counts                      junitCounts
	suites                      []junitSuite
	current                     *junitCase
	depth                       int
	seenRoot, closedRoot, valid bool
}

func (p *junitParser) warn(code string) {
	p.valid = false
	if !slices.Contains(p.out.Warnings, code) {
		p.out.Warnings = append(p.out.Warnings, code)
	}
}

func parseJUnit(data []byte) VerificationResult {
	p := junitParser{decoder: xml.NewDecoder(bytes.NewReader(data)), valid: true,
		out: VerificationResult{Source: "junit_xml_artifact", VerificationOutcome: evidenceOutcomeUnknown, Failures: []VerificationFailure{}, Warnings: []string{}}}
	for {
		token, err := p.decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			p.warn("invalid_or_incomplete_xml")
			break
		}
		if !p.consume(token) {
			break
		}
	}
	if !p.seenRoot || !p.closedRoot || p.depth != 0 || p.current != nil || len(p.suites) != 0 {
		p.warn("incomplete_report")
	}
	if p.counts.tests == 0 || p.counts.tests == p.counts.skipped {
		p.warn("no_executed_testcases")
	}
	return finishJUnit(p.out, p.counts, p.valid)
}

func (p *junitParser) consume(token xml.Token) bool {
	switch token := token.(type) {
	case xml.Directive:
		p.warn("xml_directive_not_supported")
	case xml.StartElement:
		return p.start(token)
	case xml.EndElement:
		p.end(token)
	case xml.CharData:
		if p.depth == 0 && strings.TrimSpace(string(token)) != "" {
			p.warn("text_outside_report")
		}
	}
	return true
}

func (p *junitParser) start(token xml.StartElement) bool {
	p.depth++
	if p.depth > 64 {
		p.warn("xml_nesting_limit")
		return false
	}
	if p.depth == 1 {
		if p.seenRoot || p.closedRoot || (token.Name.Local != junitSuiteTag && token.Name.Local != junitSuitesTag) {
			p.warn("unsupported_report_root")
		}
		p.seenRoot = true
	}
	switch token.Name.Local {
	case junitSuiteTag, junitSuitesTag:
		if p.current != nil || (p.depth > 1 && (len(p.suites) == 0 || p.depth != p.suites[len(p.suites)-1].depth+1)) {
			p.warn("invalid_suite_nesting")
		}
		p.suites = append(p.suites, junitSuite{before: p.counts, attributes: token.Attr, depth: p.depth, name: token.Name.Local})
	case "testcase":
		if p.current != nil || len(p.suites) == 0 || p.suites[len(p.suites)-1].name != junitSuiteTag || p.depth != p.suites[len(p.suites)-1].depth+1 {
			p.warn("invalid_testcase_nesting")
		}
		p.current = &junitCase{name: xmlAttribute(token.Attr, "name"), class: xmlAttribute(token.Attr, "classname")}
	case "failure", agentOutcomeError, junitSkippedTag:
		if p.current == nil {
			p.warn("orphan_test_result")
			return true
		}
		if token.Name.Local == junitSkippedTag {
			p.current.skipped = true
			return true
		}
		return p.failure(token)
	default:
		if err := p.decoder.Skip(); err != nil {
			p.warn("invalid_report_element")
			return false
		}
		p.depth--
	}
	return true
}

func (p *junitParser) failure(token xml.StartElement) bool {
	failure := VerificationFailure{Test: p.current.name, Class: p.current.class, Kind: "test_failure", ReportedType: xmlAttribute(token.Attr, "type"), Message: truncateUTF8(xmlAttribute(token.Attr, "message"), 2048)}
	if token.Name.Local == agentOutcomeError {
		p.current.errored = true
		failure.Kind = "test_error"
	} else {
		p.current.failed = true
	}
	if failure.ReportedType == "subprocess.TimeoutExpired" || failure.ReportedType == "TimeoutExpired" {
		failure.Kind = "subprocess_timeout"
	}
	var detail struct {
		Text string `xml:",chardata"`
	}
	if err := p.decoder.DecodeElement(&detail, &token); err != nil {
		p.warn("invalid_failure_detail")
		return false
	}
	p.depth--
	failure.Detail = truncateUTF8(detail.Text, 2048)
	if len(p.out.Failures) < 100 {
		p.out.Failures = append(p.out.Failures, failure)
	} else {
		p.out.FailuresTruncated = true
	}
	return true
}

func (p *junitParser) end(token xml.EndElement) {
	switch token.Name.Local {
	case "testcase":
		if p.current == nil {
			p.warn("invalid_testcase_nesting")
		} else {
			p.counts.tests++
			switch {
			case p.current.errored:
				p.counts.errors++
			case p.current.failed:
				p.counts.failed++
			case p.current.skipped:
				p.counts.skipped++
			default:
				p.counts.passed++
			}
			p.current = nil
		}
	case junitSuiteTag, junitSuitesTag:
		if len(p.suites) == 0 {
			p.warn("invalid_suite_nesting")
		} else {
			last := p.suites[len(p.suites)-1]
			p.suites = p.suites[:len(p.suites)-1]
			if !validateJUnitCounters(last, p.counts) {
				p.warn("declared_counter_mismatch")
			}
		}
	}
	p.depth--
	if p.depth == 0 {
		p.closedRoot = true
	}
}

func finishJUnit(out VerificationResult, counts junitCounts, valid bool) VerificationResult {
	out.Tests, out.Passed, out.Failed, out.Errors, out.Skipped = counts.tests, counts.passed, counts.failed, counts.errors, counts.skipped
	out.Complete = valid
	if valid {
		out.VerificationOutcome = "passed"
		if counts.failed > 0 || counts.errors > 0 {
			out.VerificationOutcome = "failed"
		}
	}
	return out
}
