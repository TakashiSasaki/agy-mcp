package mcptools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Mutation guards: independent event/process/report outcomes in the handlers
// and the unchanged read-only/no-collection boundary of stored evidence tools.
func TestEvidenceToolsSeparateJobAndVerification(t *testing.T) {
	state := t.TempDir()
	reports := t.TempDir()
	store := jobstore.New(state)
	const id = "mcp-evidence"
	dir, err := store.Create(jobstore.Meta{ID: id, Cwd: t.TempDir(), ArtifactDirs: []string{reports}, StartedAt: time.Now(), Args: []string{"--output-format", "stream-json"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("model says all tests passed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jobstore.WriteResultDir(dir, []byte(`{"status":"SUCCESS","response":"model says all tests passed"}`)); err != nil {
		t.Fatal(err)
	}
	journal, err := jobstore.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	journal.Append([]byte(`{"event":"init"}`))
	journal.Append([]byte(`{"event":"step_update","step_update":{"step_index":0,"step_type":"tool","execution_id":9007199254740993}}`))
	journal.Close()
	if err := jobstore.WriteProcessDir(dir, jobstore.ProcessOutcome{Source: "supervisor", EndedAt: time.Now(), ExitCode: new(0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	report := `<testsuite tests="1" errors="1"><testcase name="receive"><error type="subprocess.TimeoutExpired" message="harness limit 120 seconds"/></testcase></testsuite>`
	if err := os.WriteFile(filepath.Join(reports, "junit.xml"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(config.Config{StateDir: state, JobTTL: time.Hour, MaxConcurrency: 1})
	cs, wirePath := connectEvidenceWire(t, mgr)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		return structuredMap(t, res)
	}
	events := call(toolAgyEvents, map[string]any{"job_id": id})
	if events["state"] != "done" || events["agent_outcome"] != "success" || events["verification_outcome"] != "not_checked" || events["process"].(map[string]any)["exit_code"] != float64(0) {
		t.Fatalf("job/agent/process outcomes: %+v", events)
	}
	requireEvidenceIdentifier(t, cs, wirePath, id)
	verification := call(toolAgyVerification, map[string]any{"job_id": id, "path": "junit.xml"})
	if verification["job_state"] != "done" || verification["verification_outcome"] != "failed" || verification["errors"] != float64(1) || verification["complete"] != true {
		t.Fatalf("model narrative overrode failing evidence: %+v", verification)
	}

	requireVerificationSummary(t, cs, id, verification["sha256"])
	failure := verification["failures"].([]any)[0].(map[string]any)
	if failure["kind"] != "subprocess_timeout" || failure["reported_type"] != "subprocess.TimeoutExpired" {
		t.Fatalf("harness evidence was misattributed: %+v", failure)
	}
	digest := sha256.Sum256([]byte(report))
	if verification["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatal("report hash did not identify parsed bytes")
	}
	logs := call(toolAgyLogs, map[string]any{"job_id": id, "stream": "artifact", "path": "junit.xml", "from_start": true})
	if logs["content"] != report || logs["source"] != "artifact_file" {
		t.Fatalf("stored report bytes unavailable: %+v", logs)
	}
	listed := call(toolAgyListArtifacts, map[string]any{"job_id": id})
	if listed["scope"] != "declared" || len(listed["entries"].([]any)) != 1 {
		t.Fatalf("job-linked discovery: %+v", listed)
	}
	if mgr.Collected(id) {
		t.Fatal("evidence reads marked the final answer collected")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyLogs, Arguments: map[string]any{"job_id": id, "stream": "artifact", "path": "../../outside"}})
	if err != nil || !res.IsError {
		t.Fatalf("artifact boundary did not produce a tool error: %+v %v", res, err)
	}
}

func connectEvidenceWire(t *testing.T, mgr *manager.Manager) (session *mcp.ClientSession, wirePath string) {
	t.Helper()
	wireLog, err := os.CreateTemp(t.TempDir(), "mcp-wire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := wireLog.Close(); err != nil {
			t.Error(err)
		}
	})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := NewServer(mgr).Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "evidence-test", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.LoggingTransport{Transport: ct, Writer: wireLog}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Error(err)
		}
	})
	return cs, wireLog.Name()
}

func requireEvidenceIdentifier(t *testing.T, cs *mcp.ClientSession, wirePath, id string) {
	t.Helper()
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyEvents, Arguments: map[string]any{"job_id": id}})
	if err != nil || result.IsError {
		t.Fatalf("event wire result: %+v %v", result, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !bytes.Contains([]byte(text.Text), []byte(`"execution_id":9007199254740993`)) {
		t.Fatalf("provider identifier was rounded in the MCP result: %+v", result.Content)
	}
	wire, err := os.ReadFile(wirePath)
	if err != nil || !bytes.Contains(wire, []byte(`"execution_id":9007199254740993`)) {
		t.Fatalf("structured wire payload rounded the provider identifier: %v", err)
	}
}

func requireVerificationSummary(t *testing.T, cs *mcp.ClientSession, id string, hash any) {
	t.Helper()
	// Mutation guard: limiting details must retain failures and evidence identity.
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "junit.xml", "max_failures": 0}})
	if err != nil || res.IsError {
		t.Fatalf("summary: %+v %v", res, err)
	}
	summary := structuredMap(t, res)
	if len(summary["failures"].([]any)) != 0 || summary["failures_truncated"] != true || summary["verification_outcome"] != "failed" || summary["errors"] != float64(1) || summary["complete"] != true || summary["sha256"] != hash {
		t.Fatalf("summary changed evidence or leaked details: %+v", summary)
	}
	for _, limit := range []int{-1, 101} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "junit.xml", "max_failures": limit}})
		if err == nil && !res.IsError {
			t.Fatalf("accepted invalid max_failures %d", limit)
		}
	}
}
