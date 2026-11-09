package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func goEvents(events ...goVerificationEvent) []byte {
	var data []byte
	for _, event := range events {
		line, _ := json.Marshal(event)
		data = append(data, line...)
		data = append(data, '\n')
	}
	return data
}

func goEvent(action, pkg, test string) goVerificationEvent {
	return goVerificationEvent{Action: action, Package: pkg, Test: test}
}

func TestParseGoTestJSON(t *testing.T) {
	start := goEvent("start", "p", "")
	run := goEvent("run", "p", "TestOne")
	pass := goEvent("pass", "p", "TestOne")
	end := goEvent("pass", "p", "")
	cases := []struct {
		name     string
		data     []byte
		outcome  string
		complete bool
		tests    int
		packages int
	}{
		{"success", goEvents(start, run, pass, end), "passed", true, 1, 0},
		{"package failure after test pass", goEvents(start, run, pass, goEvent("fail", "p", "")), "failed", true, 1, 1},
		{"test failure", goEvents(start, run, goEvent("fail", "p", "TestOne"), goEvent("fail", "p", "")), "failed", true, 1, 1},
		{"repeat count", goEvents(start, run, pass, run, pass, end), "passed", true, 2, 0},
		{"parallel lifecycle", goEvents(start, run, goEvent("pause", "p", "TestOne"), goEvent("cont", "p", "TestOne"), pass, end), "passed", true, 1, 0},
		{"paused terminal", goEvents(start, run, goEvent("pause", "p", "TestOne"), pass, end), "unknown", false, 1, 0},
		{"null output", append(append(goEvents(start, run), []byte("{\"Action\":\"output\",\"Package\":\"p\",\"Output\":null}\n")...), goEvents(pass, end)...), "unknown", false, 0, 0},
		{"null test", append(append(goEvents(start, run), []byte("{\"Action\":\"output\",\"Package\":\"p\",\"Test\":null,\"Output\":\"text\"}\n")...), goEvents(pass, end)...), "unknown", false, 0, 0},
		{"empty", nil, "unknown", false, 0, 0},
		{"no tests", goEvents(start, end), "unknown", false, 0, 0},
		{"skip only", goEvents(start, run, goEvent("skip", "p", "TestOne"), end), "unknown", false, 1, 0},
		{"no test files", goEvents(start, goEvent("skip", "p", "")), "unknown", false, 0, 0},
		{"missing package end", goEvents(start, run, pass), "unknown", false, 1, 0},
		{"missing test end", goEvents(start, run, end), "unknown", false, 0, 0},
		{"contradiction", goEvents(start, run, goEvent("fail", "p", "TestOne"), end), "unknown", false, 1, 0},
		{"skip after executed test", goEvents(start, run, pass, goEvent("skip", "p", "")), "unknown", false, 1, 0},
		{"no package start", goEvents(run, pass, end), "unknown", false, 0, 0},
		{"duplicate package start", goEvents(start, start, run, pass, end), "unknown", false, 1, 0},
		{"duplicate test run", goEvents(start, run, run, pass, end), "unknown", false, 1, 0},
		{"duplicate terminal", goEvents(start, run, pass, pass, end), "unknown", false, 1, 0},
		{"event after package end", goEvents(start, run, pass, end, end), "unknown", false, 1, 0},
		{"cont without pause", goEvents(start, run, goEvent("cont", "p", "TestOne"), pass, end), "unknown", false, 1, 0},
		{"unknown package action", goEvents(start, goEvent("future", "p", ""), run, pass, end), "unknown", false, 1, 0},
		{"unknown test action", goEvents(start, run, goEvent("future", "p", "TestOne"), pass, end), "unknown", false, 1, 0},
		{"unknown benchmark", goEvents(start, goEvent("bench", "p", "BenchmarkOne"), end), "unknown", false, 0, 0},
		{"malformed tail", append(goEvents(start, run, pass, end), '{'), "unknown", false, 1, 0},
		{"non JSON tail", append(goEvents(start, run, pass, end), []byte("success")...), "unknown", false, 1, 0},
		{"duplicate key", append(goEvents(start, run, pass, end), []byte(`{"Action":"pass","Action":"fail","Package":"p"}`)...), "unknown", false, 1, 0},
		{"case duplicate key", append(goEvents(start, run, pass, end), []byte(`{"Action":"pass","action":"fail","Package":"p"}`)...), "unknown", false, 1, 0},
		{"null", []byte("null"), "unknown", false, 0, 0},
		{"array", []byte("[]"), "unknown", false, 0, 0},
		{"wrong field type", []byte(`{"Action":1}`), "unknown", false, 0, 0},
		{"invalid UTF8", append(goEvents(start, run, pass, end), 0xff), "unknown", false, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGoTestJSON(tc.data)
			if got.VerificationOutcome != tc.outcome || got.Complete != tc.complete || got.Tests != tc.tests || got.PackageFailures != tc.packages {
				t.Fatalf("got outcome=%s complete=%t tests=%d package_failures=%d warnings=%v", got.VerificationOutcome, got.Complete, got.Tests, got.PackageFailures, got.Warnings)
			}
			if got.Tests != got.Passed+got.Failed+got.Errors+got.Skipped {
				t.Fatal("test counts do not add up")
			}
		})
	}
}

func TestParseGoTestJSONBuildFailures(t *testing.T) {
	build := goVerificationEvent{Action: "build-fail", ImportPath: "p [p.test]"}
	output := goVerificationEvent{Action: "build-output", ImportPath: build.ImportPath, Output: "undefined: missingSymbol"}
	fail := goVerificationEvent{Action: "fail", Package: "p", FailedBuild: build.ImportPath}
	for _, events := range [][]goVerificationEvent{
		{output, build},
		{output, build, goEvent("start", "p", ""), fail},
		{goEvent("start", "p", ""), fail},
	} {
		got := parseGoTestJSON(goEvents(events...))
		if got.VerificationOutcome != "failed" || !got.Complete || got.Tests != 0 || got.PackageFailures != 1 || len(got.Failures) != 1 {
			t.Fatalf("build failure not preserved: %+v", got)
		}
	}
	got := parseGoTestJSON(goEvents(output, build, output))
	if got.VerificationOutcome != "unknown" {
		t.Fatalf("post-failure output should invalidate: %+v", got)
	}
	// MEASURED against Go 1.27.0 (go test -json -gcflags=-m): JSONPrinter.Printf
	// emits build-output without build-fail when compilation succeeds with
	// diagnostics. Such non-failing build-output must remain valid.
	passWithBuildDiagnostics := parseGoTestJSON(goEvents(
		output,
		goVerificationEvent{Action: "build-output", ImportPath: "p.test", Output: "# p.test\n_testmain.go:1: can inline init\n"},
		goEvent("start", "p", ""),
		goEvent("run", "p", "TestOne"),
		goEvent("pass", "p", "TestOne"),
		goEvent("pass", "p", ""),
	))
	if !passWithBuildDiagnostics.Complete || passWithBuildDiagnostics.VerificationOutcome != "passed" || passWithBuildDiagnostics.Tests != 1 || passWithBuildDiagnostics.Passed != 1 || passWithBuildDiagnostics.PackageFailures != 0 {
		t.Fatalf("non-failing build-output rejected: %+v", passWithBuildDiagnostics)
	}
	// When FailedBuild references a build that emitted build-output without a
	// terminal build-fail event, the build failure stream is incomplete.
	unfinishedFailedBuild := parseGoTestJSON(goEvents(output, goEvent("start", "p", ""), fail))
	if unfinishedFailedBuild.Complete || unfinishedFailedBuild.VerificationOutcome != "unknown" || unfinishedFailedBuild.PackageFailures != 1 {
		t.Fatalf("unfinished FailedBuild should be incomplete: %+v", unfinishedFailedBuild)
	}
	// Per Go 1.27 src/cmd/go/internal/load/printer.go, build-output always carries
	// non-empty Output and build-fail never carries Output.
	emptyBuildOutput := parseGoTestJSON(goEvents(
		goVerificationEvent{Action: "build-output", ImportPath: build.ImportPath},
		goEvent("start", "p", ""),
		goEvent("run", "p", "TestOne"),
		goEvent("pass", "p", "TestOne"),
		goEvent("pass", "p", ""),
	))
	if emptyBuildOutput.Complete || emptyBuildOutput.VerificationOutcome != "unknown" {
		t.Fatalf("empty build-output should invalidate: %+v", emptyBuildOutput)
	}
	buildFailWithOutput := parseGoTestJSON(goEvents(
		goVerificationEvent{Action: "build-fail", ImportPath: build.ImportPath, Output: "unexpected"},
	))
	if buildFailWithOutput.Complete || buildFailWithOutput.VerificationOutcome != "unknown" {
		t.Fatalf("build-fail with Output should invalidate: %+v", buildFailWithOutput)
	}
}

func TestParseGoTestJSONInterleavedPackages(t *testing.T) {
	got := parseGoTestJSON(goEvents(
		goEvent("start", "a", ""), goEvent("start", "b", ""),
		goEvent("run", "a", "TestParent"), goEvent("run", "b", "TestOne"),
		goEvent("run", "a", "TestParent/child"), goEvent("pass", "a", "TestParent/child"),
		goEvent("pass", "b", "TestOne"), goEvent("pass", "b", ""),
		goEvent("pass", "a", "TestParent"), goEvent("pass", "a", ""),
	))
	if got.VerificationOutcome != "passed" || got.Tests != 3 {
		t.Fatalf("interleaved packages: %+v", got)
	}
}

func TestParseGoTestJSONFailureBounds(t *testing.T) {
	events := make([]goVerificationEvent, 0, 317)
	events = append(events, goEvent("start", "p", ""))
	for i := range 105 {
		name := fmt.Sprintf("Test%d", i)
		events = append(events, goEvent("run", "p", name),
			goVerificationEvent{Action: "output", Package: "p", Test: name, Output: strings.Repeat("界", 3000) + "cause"},
			goEvent("fail", "p", name))
	}
	events = append(events, goEvent("fail", "p", ""))
	got := parseGoTestJSON(goEvents(events...))
	if got.VerificationOutcome != "failed" || got.Failed != 105 || len(got.Failures) != 100 || !got.FailuresTruncated {
		t.Fatalf("failure cap: outcome=%s failed=%d details=%d truncated=%t", got.VerificationOutcome, got.Failed, len(got.Failures), got.FailuresTruncated)
	}
	for _, failure := range got.Failures {
		if len(failure.Detail) > 2048 || !utf8.ValidString(failure.Detail) || !strings.HasSuffix(failure.Detail, "cause") {
			t.Fatal("failure detail must retain bounded UTF-8 tail")
		}
	}
}

func TestParseGoTestJSONMeasuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, outcome   string
		tests, packages int
	}{
		{"success", "passed", 12, 0},
		{"package-fail", "failed", 1, 1},
		{"build-fail", "failed", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "go-verification", tc.name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			got := parseGoTestJSON(data)
			if !got.Complete || got.VerificationOutcome != tc.outcome || got.Tests != tc.tests || got.PackageFailures != tc.packages {
				t.Fatalf("measured evidence: %+v", got)
			}
		})
	}
}
