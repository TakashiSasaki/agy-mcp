package mcptools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyEvents       = "agy_events"
	toolAgyLogs         = "agy_logs"
	toolAgyVerification = "agy_verification"
)

type eventsInput struct {
	JobID    string `json:"job_id" jsonschema:"job whose live or retained CLI events to retrieve"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor from the same journal generation; omit to start at the beginning"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum records scanned per page; default 50, maximum 200"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"byte budget for the events JSON array; response metadata is additional; default 65536, minimum 1024, maximum 262144; oversized payloads are omitted with an explicit flag"`
}

type logsInput struct {
	JobID     string `json:"job_id" jsonschema:"job whose log or associated artifact to read"`
	Stream    string `json:"stream,omitempty" jsonschema:"stderr (default), stdout (assistant text), events (timestamped CLI event journal), or artifact (requires path). These are not independently captured stdout/stderr of nested shell commands"`
	Path      string `json:"path,omitempty" jsonschema:"job-associated file path, required only for stream artifact; uses the same confinement as agy_read_artifact"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"resume a previous next_cursor, including at EOF to follow later appends; bound to job, stream, file identity and nearby content"`
	FromStart bool   `json:"from_start,omitempty" jsonschema:"start at byte zero; otherwise the first call reads a tail; cannot combine with cursor or tail_bytes"`
	TailBytes int    `json:"tail_bytes,omitempty" jsonschema:"initial tail window in original bytes; default max_bytes, maximum 4194304; cannot combine with cursor or from_start"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum original bytes returned per page; default 65536, minimum 4, maximum 262144; invalid UTF-8 is replaced in content and sha256 hashes original bytes"`
}

type verificationInput struct {
	ExitCodePath string `json:"exit_code_path,omitempty" jsonschema:"optional job-associated artifact containing one signed 32-bit decimal command exit code, at most 64 bytes; nonzero prevents a report-only pass, incomplete reports stay unknown; caller must associate it with the same invocation"`
	Format       string `json:"format,omitempty" jsonschema:"report format: junit (default), go_test_json for unmodified go test -json output, or tap for restricted flat numbered TAP; no filename guessing"`
	MaxFailures  *int   `json:"max_failures,omitempty" jsonschema:"maximum failure details to return (0 to 100, default 100); zero returns counts and outcome only; the complete report is always parsed"`
	JobID        string `json:"job_id" jsonschema:"job whose explicitly selected verification report to inspect"`
	Path         string `json:"path" jsonschema:"JUnit XML, Go test JSON or restricted flat TAP report within the job's associated artifact roots; maximum 4 MiB"`
}

// SDK v1.8.0 applySchema re-marshals object outputs through float64 while
// applying defaults. Validate the typed evidence separately, and return its
// original JSON as both structured content and the text fallback. Input schema
// validation still runs through the SDK. Provider IDs must not be rounded.
func addEvidenceTool[In, Out any](s *toolServer, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	schema := inferredSchema[Out]()
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(fmt.Errorf("resolve evidence output schema: %w", err))
	}
	tool.OutputSchema = schema
	addTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		result, out, err := handler(ctx, req, in)
		if err != nil {
			return nil, nil, err
		}
		data, err := json.Marshal(out)
		if err != nil {
			return nil, nil, err
		}
		// The validation view is discarded; only data goes onto the wire.
		var validationValue any
		if err := json.Unmarshal(data, &validationValue); err != nil {
			return nil, nil, err
		}
		if err := resolved.Validate(&validationValue); err != nil {
			return nil, nil, fmt.Errorf("validate evidence output: %w", err)
		}
		if result == nil {
			result = &mcp.CallToolResult{}
		}
		result.StructuredContent = json.RawMessage(data)
		result.Content = []mcp.Content{&mcp.TextContent{Text: string(data)}}
		return result, nil, nil
	})
}

func registerEvidence(s *toolServer, mgr *manager.Manager) {
	addEvidenceTool(s, &mcp.Tool{
		Name: toolAgyEvents, Title: "Read live agy events", Annotations: annReadLocal,
		Description: "Read durable, timestamped CLI events during a job or after timeout/cancellation without another model turn. Use agy_status for a compact snapshot and agy_logs for the original journal bytes or CLI stderr. Retain next_cursor at EOF to follow appends; partial records are withheld, and dropped/omitted evidence is explicit. Complete records over the structured reader limit or whose metadata alone exceeds the page budget count as skipped and later records remain readable. Use agy_logs for raw bytes. The journal is bounded to 64 MiB per job and expires with the job. Existing jobs may have no capture. process describes agy itself; payload contains only fields emitted by the CLI. Tool command arguments, exit codes and shell stdout are unknown unless the CLI emits them; a tool step marked DONE is not a successful test verdict. Use agy_verification to parse a selected test report and agy_list_artifacts to locate it.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, manager.EventsPage, error) {
		out, err := mgr.Events(manager.EventsRequest{JobID: in.JobID, Cursor: in.Cursor, Limit: in.Limit, MaxBytes: in.MaxBytes})
		return nil, out, err
	})
	addEvidenceTool(s, &mcp.Tool{
		Name: toolAgyLogs, Title: "Read an agy log page", Annotations: annReadLocal,
		Description: "Read a bounded tail or byte page of a running or retained job's stdout, stderr, event journal, or explicitly associated artifact. No model turn is started and no result is marked collected. stdout is assistant text and stderr is CLI stderr, not individual shell-command output. Use stream artifact with path for a test log within artifact_dirs, or the documented legacy roots; symlink escapes are rejected. Cursors follow appends and reject a changed file identity, job, stream or preceding content. While running, an incomplete UTF-8 suffix is withheld: pending_utf8 is true and has_more false until more bytes or job completion; retain next_cursor and wait before retrying. Use agy_events for structured events, agy_read_artifact for line ranges of stable files, and agy_list_artifacts for discovery.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, manager.LogPage, error) {
		if in.Stream == "" {
			in.Stream = "stderr"
		}
		out, err := mgr.Logs(manager.LogRequest{JobID: in.JobID, Stream: in.Stream, Path: in.Path, Cursor: in.Cursor, FromStart: in.FromStart, TailBytes: in.TailBytes, MaxBytes: in.MaxBytes})
		return nil, out, err
	})
	addEvidenceTool(s, &mcp.Tool{
		Name: toolAgyVerification, Title: "Inspect test verification evidence", Annotations: annReadLocal,
		Description: "Parse an explicitly selected JUnit XML (format junit, the default) or go test -json (format go_test_json), or restricted flat TAP (format tap) artifact into a verification outcome without an agent turn. Job state is independent: a done job can have a failed or unknown report. Malformed/truncated reports, invalid suite nesting, empty or duplicate counters, counter disagreements and reports with no executed testcases cannot pass. Go JSON requires terminal records for every observed package and testcase; package/build failures cannot pass, and malformed, truncated or unsupported records return unknown. Native Node.js JUnit testcases under testsuites are accepted; when summary comments are present, incomplete, duplicate or inconsistent summaries and unrepresented footer failures/cancellations return unknown. General JUnit does not require these comments. TAP accepts a single leading or trailing plan, sequential numbered cases, comments and SKIP directives; TODO, nested subtests, YAML diagnostics, bailouts and malformed/incomplete evidence return unknown. An explicit TimeoutExpired error type is classified as a subprocess timeout, not as proof of a product defect. SHA-256 identifies the exact report bytes. max_failures (0 to 100, default 100) bounds returned failure details; zero returns a summary while preserving complete-report validation. Optional exit_code_path selects a tiny command-status artifact: a nonzero code changes a report pass to failed, while incomplete reports remain unknown. Missing or malformed selected exit evidence is a tool error. report_outcome preserves the original verdict, and command_exit includes source, status and exact artifact hash. No job state or agent narrative supplies the code. This reads report evidence and does not independently attest that commands ran or that the file was created by this job. Use agy_list_artifacts and agy_read_artifact to inspect provenance and full details, and agy_events for CLI/process outcomes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in verificationInput) (*mcp.CallToolResult, manager.VerificationResult, error) {
		if in.MaxFailures != nil && (*in.MaxFailures < 0 || *in.MaxFailures > 100) {
			return nil, manager.VerificationResult{}, fmt.Errorf("max_failures must be between 0 and 100")
		}
		out, err := mgr.VerificationWithExitCode(in.JobID, in.Path, in.Format, in.ExitCodePath)
		if in.MaxFailures != nil && len(out.Failures) > *in.MaxFailures {
			out.Failures = out.Failures[:*in.MaxFailures]
			out.FailuresTruncated = true
		}
		return nil, out, err
	})
}
