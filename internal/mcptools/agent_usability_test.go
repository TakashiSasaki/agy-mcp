package mcptools

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func structuredMap(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	data, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T: %+v", res.StructuredContent, res)
	}
	return data
}

// Mutation guards: addStartTool's error adapter and startValidationErrors. This
// uses the SDK to exercise both handler rejection and pre-handler validation.
func TestAgentRunRejectionsAreStructured(t *testing.T) {
	cs := connect(t, nil, nil) // rejected requests must not reach the manager
	catalog, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schemas := make(map[string]*jsonschema.Resolved)
	for _, tool := range catalog.Tools {
		if tool.Name != toolAgyRun && tool.Name != toolAgyRunSync {
			continue
		}
		data, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		schemas[tool.Name] = resolved
	}
	for _, tool := range []string{toolAgyRun, toolAgyRunSync} {
		for name, args := range map[string]map[string]any{
			"missing prompt":           {},
			"wrong type":               {"prompt": 42},
			"bad timeout":              {"prompt": "task", "timeout": "bad"},
			"conflicting conversation": {"prompt": "task", "conversation_id": "x", "continue_latest": true},
		} {
			t.Run(tool+"/"+name, func(t *testing.T) {
				res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
				if err != nil || !res.IsError {
					t.Fatalf("expected tool error: %+v, %v", res, err)
				}
				out := structuredMap(t, res)
				if err := schemas[tool].Validate(out); err != nil {
					t.Fatalf("error does not satisfy the advertised output schema: %v", err)
				}
				if out["error_code"] != "invalid_argument" || out["job_started"] != false || out["retryable"] != false || out["next_action"] != "change_request" || out["job_id"] != nil {
					t.Fatalf("unsafe rejection metadata: %+v", out)
				}
				if out["message"] == "" || len(res.Content) == 0 {
					t.Fatal("original error text was lost")
				}
			})
		}
	}
	t.Run("CLI unavailable retains its cause", func(t *testing.T) {
		mgr := manager.New(config.Config{AgyPath: filepath.Join(t.TempDir(), "missing-agy"), StateDir: t.TempDir(), DefaultTimeout: time.Minute, MaxConcurrency: 1})
		res, err := connect(t, mgr, nil).CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyRun, Arguments: map[string]any{"prompt": "authorized task", "cwd": t.TempDir()}})
		if err != nil || !res.IsError {
			t.Fatalf("expected CLI tool error: %+v, %v", res, err)
		}
		out := structuredMap(t, res)
		if out["error_code"] != "agy_unavailable" || out["job_started"] != false || out["next_action"] != "check_configuration" {
			t.Fatalf("CLI failure misclassified as bad input or uncertain execution: %+v", out)
		}
	})
}

// Mutation guards: describeStartError's nullable start decision, errors.As and
// the exact versus unknown quota-mapping branches. Error text stays available.
func TestAgentStartErrorRecoveryContract(t *testing.T) {
	reset := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, code, action string
		err                error
		started            *bool
	}{
		{"capacity", "capacity_exceeded", "wait_for_capacity", &manager.StartError{Code: "capacity_exceeded", NextAction: "wait_for_capacity", JobStarted: new(false), Retryable: true, Err: errors.New("busy")}, new(false)},
		{"unknown", "start_failed", "inspect_before_retry", errors.New("storage unavailable"), nil},
		{"spawned", "start_indeterminate", "inspect_before_retry", &manager.StartError{Code: "start_indeterminate", NextAction: "inspect_before_retry", JobID: "existing", Err: errors.New("record pid")}, nil},
		{"waiting", "wait_interrupted", "inspect_job", &manager.StartError{Code: "wait_interrupted", NextAction: "inspect_job", JobID: "existing", JobStarted: new(true), Err: errors.New("wait cancelled")}, new(true)},
		{"quota", "quota_low", "skip_optional", &manager.QuotaRefusalError{Group: "Gemini Models", MatchStatus: manager.QuotaMatchExact, ResetTime: reset}, new(false)},
		{"mapping", "quota_mapping_unknown", "select_model", &manager.QuotaRefusalError{MatchStatus: manager.QuotaMatchUnknown}, new(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := describeStartError(fmt.Errorf("context: %w", tc.err))
			if out.ErrorCode != tc.code || out.NextAction != tc.action || !eqBoolPtr(out.JobStarted, tc.started) || out.Message != "context: "+tc.err.Error() {
				t.Fatalf("error decision: %+v", out)
			}
			if tc.name == "quota" && (out.RetryAfter != "2026-11-04T00:00:00Z" || out.QuotaGroup != "Gemini Models" || out.Retryable) {
				t.Fatalf("optional refusal lost quota or suggests retry: %+v", out)
			}
			if tc.name == "capacity" && !out.Retryable {
				t.Fatal("capacity refusal lost retryable decision")
			}
		})
	}
}

// Mutation guards: the summary-only handler must omit Result and skip the
// collected marker. The default handler must still return and collect it.
func TestAgentSummaryAndPagedCollection(t *testing.T) {
	mgr, id := stageTerminalJob(t)
	cs := connect(t, mgr, nil)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyStatus, Arguments: map[string]any{"job_id": id, "include_result": false}})
	if err != nil || res.IsError {
		t.Fatalf("summary: %+v, %v", res, err)
	}
	out := structuredMap(t, res)
	if out["job_id"] != id || out["result"] != nil || out["result_available"] != true || out["result_included"] != false || out["next_action"] != "get_result" || mgr.Collected(id) {
		t.Fatalf("summary leaked/collected the body or lost recovery: %+v", out)
	}
	for _, tool := range []string{toolAgyListJobs, toolAgyGetResult} {
		args := map[string]any{}
		if tool == toolAgyGetResult {
			args = map[string]any{"job_id": id, "max_bytes": 4}
		}
		res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || res.IsError || mgr.Collected(id) {
			t.Fatalf("%s collected too early: %+v, %v", tool, res, err)
		}
	}
	out = structuredMap(t, res)
	if out["content"] != "RESU" || out["complete"] != false || out["next_cursor"] == nil {
		t.Fatalf("first result page: %+v", out)
	}
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyGetResult, Arguments: map[string]any{"job_id": id, "cursor": out["next_cursor"], "max_bytes": 4}})
	if err != nil || res.IsError || !mgr.Collected(id) {
		t.Fatalf("final page did not collect: %+v, %v", res, err)
	}
	out = structuredMap(t, res)
	if out["content"] != "LT" || out["complete"] != true || out["next_cursor"] != nil {
		t.Fatalf("last result page: %+v", out)
	}
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyStatus, Arguments: map[string]any{"job_id": id}})
	if err != nil || res.IsError || structuredMap(t, res)["result"] != "RESULT" || structuredMap(t, res)["result_included"] != true {
		t.Fatalf("legacy default changed: %+v, %v", res, err)
	}
}

// Mutation guards: resultPage's UTF-8 boundary, limits, and snapshot comparison.
// Pagination complete is deliberately independent of job success and partial.
func TestAgentResultCursorAndUTF8(t *testing.T) {
	st := manager.Status{JobID: "one", State: manager.StateFailed, FailureReason: manager.ReasonTimeout, Partial: true, Result: "あ😀éZ日本語"}
	var collected strings.Builder
	cursor := ""
	for range 20 {
		out, err := resultPage(st, resultInput{JobID: st.JobID, Cursor: cursor, MaxBytes: 4})
		if err != nil || !utf8.ValidString(out.Content) || len(out.Content) > 4 || out.Content == "" || !out.Partial || out.State != manager.StateFailed || out.FailureReason != manager.ReasonTimeout {
			t.Fatalf("UTF-8 page/provenance: content=%q state=%q partial=%t failure=%q error=%v", out.Content, out.State, out.Partial, out.FailureReason, err)
		}
		collected.WriteString(out.Content)
		if out.Complete {
			break
		}
		cursor = out.NextCursor
	}
	if collected.String() != st.Result {
		t.Fatalf("pages changed the result: %q", collected.String())
	}
	first, err := resultPage(st, resultInput{MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*manager.Status){
		func(s *manager.Status) { s.JobID = "two" },
		func(s *manager.Status) { s.Result += "changed" },
		func(s *manager.Status) { s.Partial = false },
		func(s *manager.Status) { s.State = manager.StateDone },
	} {
		changed := st
		change(&changed)
		if _, err := resultPage(changed, resultInput{Cursor: first.NextCursor}); err == nil {
			t.Errorf("accepted a cursor for changed snapshot: %+v", changed)
		}
	}
	data, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var decoded resultCursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{-1, 1, len(st.Result) + 1} {
		decoded.Offset = offset
		data, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resultPage(st, resultInput{Cursor: base64.RawURLEncoding.EncodeToString(data)}); err == nil {
			t.Errorf("accepted invalid byte offset %d", offset)
		}
	}
	for _, invalid := range []resultInput{{Cursor: "bad!"}, {Cursor: strings.Repeat("x", 4097)}, {MaxBytes: -1}, {MaxBytes: 3}, {MaxBytes: maxResultBytes + 1}} {
		if _, err := resultPage(st, invalid); err == nil {
			t.Errorf("accepted invalid request %+v", invalid)
		}
	}
	running, err := resultPage(manager.Status{JobID: "one", State: manager.StateRunning}, resultInput{})
	if err != nil || running.Complete || running.NextAction != "wait" || running.ResultSHA256 != "" {
		t.Fatalf("running result: %+v, %v", running, err)
	}
}

// Mutation guard: catalogue retention/canonicalization and info's actual runtime
// projection. A missing CLI is a diagnostic value, not an invocation failure.
func TestAgentInfoMatchesVisibleCatalog(t *testing.T) {
	mgr := manager.New(config.Config{StateDir: t.TempDir(), AgyPath: filepath.Join(t.TempDir(), "missing-agy"), DefaultTimeout: time.Minute, MaxConcurrency: 4})
	cs := connect(t, mgr, nil)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyInfo, Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("info: %+v, %v", res, err)
	}
	out := structuredMap(t, res)
	if out["catalog_sha256"] != catalogDigest(tools.Tools) || out["tool_count"] != float64(len(tools.Tools)) || out["api_version"] != "1" {
		t.Fatalf("info does not identify the actual catalog: %+v", out)
	}
	runtime, ok := out["runtime"].(map[string]any)
	if !ok || runtime["auth_status"] != "not_checked" || runtime["read_only_enforced"] != false || runtime["concurrency_scope"] != "process" || runtime["default_timeout"] != "1m0s" {
		t.Fatalf("runtime defaults/capabilities: %+v", runtime)
	}
}

// Mutation guard: paged retrieval retains failed/partial classification rather
// than treating a successfully retrieved page as a successfully completed job.
func TestAgentResultRetrievalKeepsFailure(t *testing.T) {
	stateDir := t.TempDir()
	store := jobstore.New(stateDir)
	const id = "partial-job"
	dir, err := store.Create(jobstore.Meta{ID: id, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("RESULT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, jobstore.ExitTimeout); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(config.Config{StateDir: stateDir, MaxConcurrency: 1})
	res, err := connect(t, mgr, nil).CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyGetResult, Arguments: map[string]any{"job_id": id}})
	if err != nil || res.IsError {
		t.Fatalf("retrieving a failed job is an ordinary tool success: %+v, %v", res, err)
	}
	out := structuredMap(t, res)
	if out["state"] != manager.StateFailed || out["failure_reason"] != manager.ReasonTimeout || out["partial"] != true || out["complete"] != true || out["content"] != "RESULT" {
		t.Fatalf("failed partial answer lost: %+v", out)
	}
}

// Mutation guard: statusNextAction's non-retryable verdict precedes quota
// waiting advice; a quota category alone cannot establish a future reset.
func TestAgentNextActionRespectsJobVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status manager.Status
		want   string
	}{
		{"running", manager.Status{State: manager.StateRunning}, "wait"},
		{"done", manager.Status{State: manager.StateDone}, "use_result"},
		{"partial", manager.Status{State: manager.StateFailed, Result: "text", Partial: true}, "inspect_partial_result"},
		{"auth", manager.Status{State: manager.StateFailed, SignInRequired: true, Retryable: new(false)}, "fix_auth"},
		{"permanent", manager.Status{State: manager.StateFailed, Retryable: new(false)}, "change_request"},
		{"quota permanent", manager.Status{State: manager.StateFailed, FailureReason: manager.ReasonQuotaExhausted, Retryable: new(false)}, "inspect_before_retry"},
		{"quota temporary", manager.Status{State: manager.StateFailed, FailureReason: manager.ReasonQuotaExhausted, Retryable: new(true)}, "wait_for_quota_reset"},
		{"cancelled", manager.Status{State: manager.StateCancelled}, "inspect_before_retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusNextAction(tc.status); got != tc.want {
				t.Errorf("next_action = %q, want %q", got, tc.want)
			}
		})
	}
}
