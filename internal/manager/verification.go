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
	Test         string `json:"test" jsonschema:"testcase name reported in the artifact, bounded to 2048 UTF-8 bytes; empty for a Go package/build failure"`
	Class        string `json:"class,omitempty" jsonschema:"JUnit testcase classname or Go package/import path, bounded to 2048 UTF-8 bytes"`
	Kind         string `json:"kind" jsonschema:"test_failure, test_error, package_failure, build_failure or subprocess_timeout; the latter requires an explicit TimeoutExpired exception type"`
	ReportedType string `json:"reported_type,omitempty" jsonschema:"failure/error type from XML, bounded to 2048 UTF-8 bytes; classification uses the full type; not a model diagnosis of the product"`
	Message      string `json:"message,omitempty" jsonschema:"reported message, bounded to 2048 UTF-8 bytes"`
	Detail       string `json:"detail,omitempty" jsonschema:"reported failure text, bounded to 2048 UTF-8 bytes; full details remain in the artifact"`
}

type VerificationResult struct {
	ReportOutcome       string                `json:"report_outcome,omitempty" jsonschema:"original parsed-report verdict when optional command exit evidence was selected"`
	CommandExit         *CommandExitEvidence  `json:"command_exit,omitempty" jsonschema:"explicitly selected command exit evidence; does not attest which invocation produced either artifact"`
	JobID               string                `json:"job_id" jsonschema:"job whose associated artifact was inspected"`
	JobState            string                `json:"job_state" jsonschema:"independent job lifecycle state; does not set the verification outcome"`
	Source              string                `json:"source" jsonschema:"junit_xml_artifact, go_test_json_artifact or tap_artifact; this is a parsed report, not independently witnessed command execution"`
	Scope               string                `json:"scope" jsonschema:"declared or legacy_workspace artifact read scope"`
	Path                string                `json:"path" jsonschema:"requested report path within job-associated roots"`
	SHA256              string                `json:"sha256" jsonschema:"SHA-256 of the exact report bytes parsed"`
	SizeBytes           int                   `json:"size_bytes" jsonschema:"size of the report parsed, bounded to 4 MiB"`
	VerificationOutcome string                `json:"verification_outcome" jsonschema:"passed, failed or unknown according to the report and optional command exit evidence; never inferred from job_state"`
	Complete            bool                  `json:"complete" jsonschema:"report structure and terminal records or declared counters agree; empty or incomplete evidence cannot pass"`
	Tests               int                   `json:"tests" jsonschema:"testcases observed in the parsed report"`
	Passed              int                   `json:"passed" jsonschema:"observed testcases without failure, error or skipped markers"`
	Failed              int                   `json:"failed" jsonschema:"observed testcases with failure markers"`
	Errors              int                   `json:"errors" jsonschema:"observed testcases with error markers"`
	Skipped             int                   `json:"skipped" jsonschema:"observed skipped testcases"`
	Failures            []VerificationFailure `json:"failures" jsonschema:"up to 100 reported failure/error details"`
	FailuresTruncated   bool                  `json:"failures_truncated" jsonschema:"more details exist in the report than were included here"`
	PackageFailures     int                   `json:"package_failures,omitempty" jsonschema:"Go package or build failures, separate from testcase counts; zero for JUnit reports"`
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
	name, class                        string
	failed, errored, skipped, disabled bool
}

// Verification preserves the original JUnit-only manager entry point.
func (m *Manager) Verification(jobID, path string) (VerificationResult, error) {
	return m.VerificationReport(jobID, path, "junit")
}

// VerificationReport parses an explicit format without guessing from filenames.
func (m *Manager) VerificationReport(jobID, path, format string) (VerificationResult, error) {
	if format == "" {
		format = "junit"
	}
	if format != "junit" && format != "go_test_json" && format != "tap" {
		return VerificationResult{}, fmt.Errorf("format must be junit, go_test_json or tap")
	}
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
	var out VerificationResult
	switch format {
	case "go_test_json":
		out = parseGoTestJSON(data)
	case "tap":
		out = parseFlatTAP(data)
	default:
		out = parseJUnit(data)
	}
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
		if name == "disabled" {
			if seen[name] {
				return false
			}
			got, err := strconv.Atoi(attr.Value)
			if err != nil || got != 0 {
				return false
			}
			seen[name] = true
			continue
		}
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
	nodeSummary                 map[string]int
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
	if p.counts.tests == 0 || p.counts.passed+p.counts.failed+p.counts.errors == 0 {
		p.warn("no_executed_testcases")
	}
	p.validateNodeSummary()
	return finishJUnit(p.out, p.counts, p.valid)
}

func (p *junitParser) consume(token xml.Token) bool {
	switch token := token.(type) {
	case xml.Comment:
		p.nodeComment(string(token))
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

// MEASURED against Node.js v24.22.2: parent assertions can fail after a
// passing child while the JUnit reporter emits only that child as a testcase.
// Its root-level summary comments retain the otherwise missing failure. Parent
// tests also increase summary totals, so they need not equal testcase counts.
func (p *junitParser) nodeComment(comment string) {
	if p.depth != 1 || len(p.suites) != 1 || p.suites[0].name != junitSuitesTag {
		return
	}
	fields := strings.Fields(comment)
	if len(fields) == 0 || !slices.Contains([]string{"tests", "suites", "pass", "fail", "cancelled", "skipped", "todo"}, fields[0]) {
		return
	}
	if p.nodeSummary == nil {
		p.nodeSummary = make(map[string]int)
	}
	if _, duplicate := p.nodeSummary[fields[0]]; duplicate {
		p.warn("invalid_node_summary")
	}
	if len(fields) != 2 {
		p.warn("invalid_node_summary")
		return
	}
	value, err := strconv.Atoi(fields[1])
	if err != nil || value < 0 {
		p.warn("invalid_node_summary")
		return
	}
	p.nodeSummary[fields[0]] = value
}

func (p *junitParser) validateNodeSummary() {
	if p.nodeSummary == nil {
		return // General JUnit does not require Node-specific comments.
	}
	if len(p.nodeSummary) != 7 {
		p.warn("incomplete_node_summary")
		return
	}
	total, accounted := p.nodeSummary["tests"], 0
	for _, key := range []string{"pass", "fail", "cancelled", "skipped", "todo"} {
		value := p.nodeSummary[key]
		if value > total-accounted {
			p.warn("inconsistent_node_summary")
			return
		}
		accounted += value
	}
	if accounted != total || total < p.counts.tests {
		p.warn("inconsistent_node_summary")
	}
	// Todo failures may be emitted as both skipped and failure elements, so do
	// not downgrade explicit failure evidence merely because footer fail is 0.
	represented := p.counts.failed + p.counts.errors
	if p.nodeSummary["fail"] > represented || p.nodeSummary["cancelled"] > represented-p.nodeSummary["fail"] {
		p.warn("unrepresented_node_failure")
	}
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
		if d := xmlAttribute(token.Attr, "disabled"); d != "" {
			if got, err := strconv.Atoi(d); err == nil && got > 0 {
				p.warn("unsupported_disabled_tests")
			}
		}
		p.suites = append(p.suites, junitSuite{before: p.counts, attributes: token.Attr, depth: p.depth, name: token.Name.Local})
	case "testcase":
		// MEASURED against Node.js v24.22.2: its built-in JUnit reporter writes
		// top-level testcases directly under the root testsuites element.
		rootCases := len(p.suites) == 1 && p.suites[0].name == junitSuitesTag && p.suites[0].depth == 1
		if p.current != nil || len(p.suites) == 0 || (!rootCases && p.suites[len(p.suites)-1].name != junitSuiteTag) || p.depth != p.suites[len(p.suites)-1].depth+1 {
			p.warn("invalid_testcase_nesting")
		}
		p.current = &junitCase{
			name:     xmlAttribute(token.Attr, "name"),
			class:    xmlAttribute(token.Attr, "classname"),
			disabled: p.disabledCase(token.Attr),
		}
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

// GoogleTest v1.16.0 emits notrun/suppressed for disabled cases. Read every
// execution attribute: encoding/xml does not reject duplicate attributes.
func (p *junitParser) disabledCase(attrs []xml.Attr) bool {
	seen := make(map[string]bool)
	disabled := false
	for _, attr := range attrs {
		name := attr.Name.Local
		if name != "status" && name != "result" {
			continue
		}
		if seen[name] {
			p.warn("duplicate_testcase_execution_attribute")
		}
		seen[name] = true
		if (name == "status" && attr.Value == "notrun") || (name == "result" && attr.Value == "suppressed") {
			disabled = true
		}
	}
	if disabled {
		p.warn("unsupported_disabled_tests")
	}
	return disabled
}

func (p *junitParser) failure(token xml.StartElement) bool {
	reportedType := xmlAttribute(token.Attr, "type")
	failure := VerificationFailure{
		Test: truncateUTF8(p.current.name, 2048), Class: truncateUTF8(p.current.class, 2048),
		Kind: "test_failure", ReportedType: truncateUTF8(reportedType, 2048),
		Message: truncateUTF8(xmlAttribute(token.Attr, "message"), 2048),
	}
	if token.Name.Local == agentOutcomeError {
		p.current.errored = true
		failure.Kind = "test_error"
	} else {
		p.current.failed = true
	}
	if reportedType == "subprocess.TimeoutExpired" || reportedType == "TimeoutExpired" {
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
			case p.current.disabled:
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
