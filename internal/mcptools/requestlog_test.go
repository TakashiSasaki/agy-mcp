package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const requestLogTestJobID = "1793710756399705834-cfbbeb0c282a115720f6ff18"

func readRequestLogs(t *testing.T, buf *bytes.Buffer) []requestLogEntry {
	t.Helper()
	var entries []requestLogEntry
	for line := range strings.Lines(buf.String()) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry requestLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid request log JSON: %v: %s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

// Removing requestLoggingMiddleware's addResult call must turn the failed-job
// row red: successful MCP delivery and successful delegated work are distinct.
func TestRequestLoggingOutcomesAndPrivacy(t *testing.T) {
	t.Parallel()
	protocolErr := errors.New("secret-error-detail")
	for _, tc := range []struct {
		name, outcome, state, reason string
		result                       mcp.Result
		err                          error
		partial                      bool
	}{
		{name: "failed job in successful call", outcome: "ok", state: manager.StateFailed, reason: manager.ReasonTimeout, partial: true,
			result: &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"state":"failed","failure_reason":"timeout","partial":true,"result":"secret-result","error":"secret-error-detail"}`)}},
		{name: "running job", outcome: "ok", state: manager.StateRunning,
			result: &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"state":"running"}`)}},
		{name: "tool error", outcome: "tool_error", result: &mcp.CallToolResult{IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "secret-result"}}}},
		{name: "protocol error", outcome: "protocol_error", err: protocolErr},
		{name: "cancelled", outcome: "cancelled", err: context.Canceled},
		{name: "deadline", outcome: "deadline_exceeded", err: context.DeadlineExceeded},
		{name: "malformed metadata", outcome: "ok", result: &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"state":`)}},
		{name: "untrusted categories", outcome: "ok", state: manager.StateFailed, reason: manager.ReasonUnknown,
			result: &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"state":"failed","failure_reason":"secret-category"}`)}},
		{name: "untrusted state", outcome: "ok", result: &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"state":"secret-state"}`)}},
		{name: "nil result", outcome: "ok"},
		{name: "typed nil result", outcome: "ok", result: (*mcp.CallToolResult)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolAgyStatus,
				Arguments: json.RawMessage(`{"job_id":"` + requestLogTestJobID + `","prompt":"secret-prompt"}`)},
				Extra: &mcp.RequestExtra{Header: http.Header{"Authorization": {"Bearer secret-header"}}}}
			wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(_ context.Context, method string, got mcp.Request) (mcp.Result, error) {
				if method != "tools/call" || got != req {
					t.Fatal("middleware changed the request")
				}
				return tc.result, tc.err
			})
			result, err := wrapped(t.Context(), "tools/call", req)
			if result != tc.result || !errors.Is(err, tc.err) {
				t.Fatal("middleware changed the result or error")
			}
			entries := readRequestLogs(t, &buf)
			if len(entries) != 2 {
				t.Fatalf("got %d records, want a start and a completion", len(entries))
			}
			start, done := entries[0], entries[1]
			if start.Phase != "started" || done.Phase != "completed" || start.CallID == 0 || start.CallID != done.CallID || start.PID != os.Getpid() {
				t.Fatalf("uncorrelated request records: %+v", entries)
			}
			if done.Event != "mcp_request" || done.Tool != toolAgyStatus || done.JobID != requestLogTestJobID || done.ElapsedMS == nil || *done.ElapsedMS < 0 {
				t.Fatalf("missing request metadata: %+v", done)
			}
			if done.Outcome != tc.outcome || done.JobState != tc.state || done.FailureReason != tc.reason || done.Partial != tc.partial {
				t.Fatalf("completion = %+v, want outcome %q, job state %q, failure %q, partial %v", done, tc.outcome, tc.state, tc.reason, tc.partial)
			}
			if strings.Contains(buf.String(), "secret-") {
				t.Fatalf("request log leaked private payload: %s", &buf)
			}
		})
	}
}

func TestRequestLoggingRejectsUntrustedIdentifiers(t *testing.T) {
	t.Parallel()
	for _, jobID := range []string{"../secret-path", strings.Repeat("1", 200) + "-cfbbeb0c282a115720f6ff18"} {
		var buf bytes.Buffer
		args, err := json.Marshal(map[string]string{"job_id": jobID})
		if err != nil {
			t.Fatal(err)
		}
		wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return &mcp.CallToolResult{IsError: true}, nil
		})
		_, err = wrapped(t.Context(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "secret-tool\nforged log", Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range readRequestLogs(t, &buf) {
			if entry.Tool != "unknown" || entry.JobID != "" || strings.Contains(buf.String(), "secret-") {
				t.Fatalf("untrusted identifiers entered the log: %s", &buf)
			}
		}
	}
}

func TestRequestLoggingCatalogFingerprint(t *testing.T) {
	t.Parallel()
	a := &mcp.Tool{Name: "a", InputSchema: map[string]any{"type": "object"}}
	b := &mcp.Tool{Name: "b", InputSchema: map[string]any{"type": "object"}}
	tools := []*mcp.Tool{b, a}
	first := requestLogEntry{}
	first.addResult(&mcp.ListToolsResult{Tools: tools})
	second := requestLogEntry{}
	second.addResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{a, b}, NextCursor: "secret-cursor"})
	if first.CatalogSHA256 == "" || first.CatalogSHA256 != second.CatalogSHA256 || *first.ToolCount != 2 || !*first.CatalogComplete || *second.CatalogComplete {
		t.Fatalf("catalog pagination/order handling: first=%+v second=%+v", first, second)
	}
	if tools[0] != b || tools[1] != a {
		t.Fatal("fingerprinting changed the response's tool order")
	}
	changed := &mcp.Tool{Name: "b", InputSchema: map[string]any{"type": "object", "required": []string{"cwd"}}}
	third := requestLogEntry{}
	third.addResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{a, changed}})
	if first.CatalogSHA256 == third.CatalogSHA256 {
		t.Fatal("schema change did not change catalog fingerprint")
	}
	// Logging must not panic or change a handler's result on malformed catalog
	// data. The transport, not the logger, owns validation of that result.
	third.addResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{nil, a, nil}})
	third.addResult((*mcp.ListToolsResult)(nil))
}

func TestRequestLoggingCatalogContinuationIsIncomplete(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: toolAgyStatus}}}, nil
	})
	_, err := wrapped(t.Context(), "tools/list", &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{Cursor: "secret-cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	entries := readRequestLogs(t, &buf)
	if len(entries) != 2 || entries[1].CatalogComplete == nil || *entries[1].CatalogComplete || strings.Contains(buf.String(), "secret-cursor") {
		t.Fatalf("final page was mistaken for the full catalog or leaked its cursor: %s", &buf)
	}
}

func TestRequestLoggingNewJobIDFromResult(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"job_id":"` + requestLogTestJobID + `","state":"running"}`)}, nil
	})
	_, err := wrapped(t.Context(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolAgyRun, Arguments: json.RawMessage(`{"prompt":"secret-prompt"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	entries := readRequestLogs(t, &buf)
	if len(entries) != 2 || entries[0].JobID != "" || entries[1].JobID != requestLogTestJobID || entries[1].JobState != manager.StateRunning || strings.Contains(buf.String(), "secret-prompt") {
		t.Fatalf("new job was not linked to the completed call: %s", &buf)
	}
}

func TestRequestLoggingConcurrentCalls(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			_, _ = wrapped(t.Context(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolListSessions}})
		})
	}
	wg.Wait()
	counts := make(map[uint64]int)
	for _, entry := range readRequestLogs(t, &buf) {
		counts[entry.CallID]++
	}
	if len(counts) != 40 {
		t.Fatalf("got %d distinct call IDs, want 40", len(counts))
	}
	for id, count := range counts {
		if count != 2 {
			t.Fatalf("call %d has %d records, want 2", id, count)
		}
	}
}

func TestRequestLoggingPanicPropagates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	wrapped := requestLoggingMiddleware(log.New(&buf, "", 0))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic("secret-panic")
	})
	func() {
		defer func() {
			if got := recover(); got != "secret-panic" {
				t.Fatalf("panic changed: %v", got)
			}
		}()
		_, _ = wrapped(t.Context(), "tools/call", &mcp.CallToolRequest{})
	}()
	entries := readRequestLogs(t, &buf)
	if len(entries) != 2 || entries[1].Outcome != "panic" || strings.Contains(buf.String(), "secret-panic") {
		t.Fatalf("panic logging lost completion or exposed the panic text: %s", &buf)
	}
}

// Removing NewServer's AddReceivingMiddleware registration must turn this test
// red. Exercise the real SDK adapter's raw structured output and a disk-backed
// timeout without launching agy or a background supervisor.
func TestRequestLoggingRealServer(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := jobstore.New(stateDir)
	if _, err := store.Create(jobstore.Meta{ID: requestLogTestJobID, StartedAt: time.Now(), Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(requestLogTestJobID, jobstore.ExitTimeout); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(config.Config{StateDir: stateDir, DefaultTimeout: time.Minute, MaxConcurrency: 4})
	var buf bytes.Buffer
	srv := newServer(mgr, log.New(&buf, "", 0))
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "request-log-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyStatus, Arguments: map[string]any{"job_id": requestLogTestJobID}})
	if err != nil || result.IsError || stateOf(t, result.StructuredContent) != manager.StateFailed {
		t.Fatalf("timeout status not returned successfully: result=%+v err=%v", result, err)
	}
	foundCatalog, foundStatus := false, false
	for _, entry := range readRequestLogs(t, &buf) {
		if entry.Phase != "completed" {
			continue
		}
		if entry.Method == "tools/list" {
			foundCatalog = entry.ToolCount != nil && *entry.ToolCount == len(listed.Tools) && entry.CatalogSHA256 != ""
		}
		if entry.Tool == toolAgyStatus {
			foundStatus = entry.Outcome == "ok" && entry.JobID == requestLogTestJobID && entry.JobState == manager.StateFailed && entry.FailureReason == manager.ReasonTimeout
		}
	}
	if !foundCatalog || !foundStatus {
		t.Fatalf("missing real-server catalog/failed-job diagnostics: %s", &buf)
	}
}
