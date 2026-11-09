package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	goActionOutput      = "output"
	goActionPass        = "pass"
	goActionFail        = "fail"
	goActionSkip        = "skip"
	goActionBuildOutput = "build-output"
	goActionBuildFail   = "build-fail"
)

// Event contracts follow Go 1.27.3 src/cmd/test2json/main.go and
// src/cmd/go/internal/load/printer.go. This validates report evidence only.
type goVerificationEvent struct {
	Action, Package, Test, Output, ImportPath, FailedBuild string
}

type goVerificationTest struct {
	active, paused bool
	output         string
}

type goVerificationPackage struct {
	ended, failed bool
	output        string
	tests         map[string]*goVerificationTest
}

type goVerificationBuild struct {
	failed, referenced bool
	output             string
}

type goVerificationParser struct {
	out      VerificationResult
	packages map[string]*goVerificationPackage
	builds   map[string]*goVerificationBuild
	counts   junitCounts
	valid    bool
}

func parseGoTestJSON(data []byte) VerificationResult {
	p := goVerificationParser{
		out: VerificationResult{Source: "go_test_json_artifact", VerificationOutcome: evidenceOutcomeUnknown,
			Failures: []VerificationFailure{}, Warnings: []string{}},
		packages: make(map[string]*goVerificationPackage), builds: make(map[string]*goVerificationBuild), valid: true,
	}
	if !utf8.Valid(data) {
		p.warn("invalid_go_json_encoding")
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		event, err := decodeGoVerificationEvent(line)
		if err != nil {
			p.warn("invalid_go_json_event")
			break
		}
		p.consume(event)
	}
	return p.finish()
}

// Unmarshal alone silently accepts duplicate keys and JSON null. Reject both
// before decoding known fields; unknown fields remain forward compatible.
func decodeGoVerificationEvent(line []byte) (goVerificationEvent, error) {
	d := json.NewDecoder(bytes.NewReader(line))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return goVerificationEvent{}, errors.New("expected event object")
	}
	seen := make(map[string]bool)
	for d.More() {
		key, keyErr := d.Token()
		if keyErr != nil {
			return goVerificationEvent{}, keyErr
		}
		name, ok := key.(string)
		if !ok || seen[strings.ToLower(name)] {
			return goVerificationEvent{}, errors.New("duplicate event field")
		}
		seen[strings.ToLower(name)] = true
		var raw json.RawMessage
		if decodeErr := d.Decode(&raw); decodeErr != nil {
			return goVerificationEvent{}, decodeErr
		}
		switch strings.ToLower(name) {
		case "action", "package", "test", goActionOutput, "importpath", "failedbuild":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return goVerificationEvent{}, errors.New("null event string field")
			}
		}
	}
	if _, err = d.Token(); err != nil {
		return goVerificationEvent{}, err
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return goVerificationEvent{}, errors.New("trailing event content")
	}
	var event goVerificationEvent
	err = json.Unmarshal(line, &event)
	return event, err
}

func (p *goVerificationParser) warn(code string) {
	p.valid = false
	if !slices.Contains(p.out.Warnings, code) {
		p.out.Warnings = append(p.out.Warnings, code)
	}
}

func goVerificationTail(previous, next string) string {
	combined := previous + next
	const limit = 2048
	if len(combined) <= limit {
		return combined
	}
	start := len(combined) - limit
	for start < len(combined) && !utf8.RuneStart(combined[start]) {
		start++
	}
	return combined[start:]
}

func (p *goVerificationParser) failure(test, class, kind, detail string) {
	if len(p.out.Failures) >= 100 {
		p.out.FailuresTruncated = true
		return
	}
	p.out.Failures = append(p.out.Failures, VerificationFailure{
		Test: truncateUTF8(test, 2048), Class: truncateUTF8(class, 2048), Kind: kind,
		Detail: truncateUTF8(detail, 2048),
	})
}

func (p *goVerificationParser) consume(event goVerificationEvent) {
	if event.Action == goActionBuildOutput || event.Action == goActionBuildFail {
		p.build(event)
		return
	}
	if event.Package == "" || event.ImportPath != "" || (event.FailedBuild != "" && (event.Action != goActionFail || event.Test != "")) {
		p.warn("invalid_go_event_scope")
		return
	}
	pkg := p.packages[event.Package]
	if event.Action == "start" {
		if pkg != nil || event.Test != "" {
			p.warn("duplicate_or_invalid_go_package_start")
			return
		}
		p.packages[event.Package] = &goVerificationPackage{tests: make(map[string]*goVerificationTest)}
		return
	}
	if pkg == nil || pkg.ended {
		p.warn("go_event_outside_package_lifecycle")
		return
	}
	if event.Test != "" {
		p.test(pkg, event)
		return
	}
	switch event.Action {
	case goActionOutput:
		pkg.output = goVerificationTail(pkg.output, event.Output)
	case goActionPass, goActionFail, goActionSkip:
		p.endPackage(pkg, event)
	default:
		p.warn("unsupported_go_package_action")
	}
}

func (p *goVerificationParser) build(event goVerificationEvent) {
	if event.ImportPath == "" || event.Package != "" || event.Test != "" || event.FailedBuild != "" ||
		(event.Action == goActionBuildOutput && event.Output == "") || (event.Action == goActionBuildFail && event.Output != "") {
		p.warn("invalid_go_build_event")
		return
	}
	build := p.builds[event.ImportPath]
	if build == nil {
		build = &goVerificationBuild{}
		p.builds[event.ImportPath] = build
	}
	if build.failed {
		p.warn("go_event_after_build_failure")
		return
	}
	build.output = goVerificationTail(build.output, event.Output)
	build.failed = event.Action == goActionBuildFail
}

func (p *goVerificationParser) test(pkg *goVerificationPackage, event goVerificationEvent) {
	test := pkg.tests[event.Test]
	if event.Action == "run" {
		if test != nil && test.active {
			p.warn("duplicate_go_test_run")
			return
		}
		pkg.tests[event.Test] = &goVerificationTest{active: true}
		return
	}
	if test == nil || !test.active {
		p.warn("go_event_outside_test_lifecycle")
		return
	}
	switch event.Action {
	case goActionOutput:
		test.output = goVerificationTail(test.output, event.Output)
	case "pause":
		if test.paused {
			p.warn("duplicate_go_test_pause")
		}
		test.paused = true
	case "cont":
		if !test.paused {
			p.warn("go_test_continue_without_pause")
		}
		test.paused = false
	case goActionPass, goActionFail, goActionSkip:
		if test.paused {
			p.warn("go_test_finished_while_paused")
		}
		test.active = false
		p.counts.tests++
		switch event.Action {
		case goActionPass:
			p.counts.passed++
		case goActionSkip:
			p.counts.skipped++
		case goActionFail:
			p.counts.failed++
			pkg.failed = true
			p.failure(event.Test, event.Package, "test_failure", test.output)
		}
	default:
		p.warn("unsupported_go_test_action")
	}
}

func (p *goVerificationParser) endPackage(pkg *goVerificationPackage, event goVerificationEvent) {
	pkg.ended = true
	for _, test := range pkg.tests {
		if test.active {
			p.warn("unfinished_go_test")
		}
	}
	if (pkg.failed && event.Action != goActionFail) || (event.Action == goActionSkip && len(pkg.tests) != 0) {
		p.warn("contradictory_go_package_result")
	}
	if event.Action == goActionFail {
		p.out.PackageFailures++
		detail := pkg.output
		if event.FailedBuild != "" {
			if build := p.builds[event.FailedBuild]; build != nil {
				if !build.failed {
					p.warn("unfinished_go_build")
				}
				build.referenced = true
				detail = goVerificationTail(build.output, detail)
			}
		}
		p.failure("", event.Package, "package_failure", detail)
	}
}

func (p *goVerificationParser) finish() VerificationResult {
	for _, pkg := range p.packages {
		if !pkg.ended {
			p.warn("unfinished_go_package")
		}
	}
	for name, build := range p.builds {
		if build.failed && !build.referenced {
			p.out.PackageFailures++
			p.failure("", name, "build_failure", build.output)
		}
	}
	if p.counts.tests == p.counts.skipped && p.out.PackageFailures == 0 {
		p.warn("no_executed_testcases")
	}
	out := finishJUnit(p.out, p.counts, p.valid)
	if out.Complete && out.PackageFailures > 0 {
		out.VerificationOutcome = "failed"
	}
	return out
}
