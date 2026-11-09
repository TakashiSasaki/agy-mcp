package mcptools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// waitInput is the input for agy_wait.
type waitInput struct {
	IncludeResult *bool  `json:"include_result,omitempty" jsonschema:"include terminal result body (default true); false returns metadata without collecting the job; retrieve text with agy_get_result"`
	JobID         string `json:"job_id" jsonschema:"job id to wait for, as returned by agy_run or agy_run_sync"`
	Wait          string `json:"wait,omitempty" jsonschema:"maximum time this call waits for execution and any required cloud sync, as a positive Go duration. Omitted or empty uses the effective ceiling (default 90s, see agy_info.max_inline_wait). Larger values clamp to the ceiling. If either phase is pending after expiry, use agy_wait again or check agy_status for the same job_id"`
}

// registerWait adds the agy_wait tool: block on an existing job until it
// finishes (bounded), streaming progress notifications when the client asked
// for them. It shares agy_run_sync's wait phase, so one agy_wait call
// replaces an agy_status poll loop.
func registerWait(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyWait,
		Title:       "Wait for an agy job",
		Annotations: annReadLocal,
		Description: "Block until execution and required cloud conversation sync finish (bounded by wait). " +
			"Accepts any job_id, whether it came from agy_run or from an agy_run_sync that " +
			"outlived its inline wait. Prefer one agy_wait over repeated agy_status polling " +
			"when the next step depends on the job's result; use agy_status instead for a " +
			"single non-blocking check. If the connection delays other calls behind a long wait, " +
			"use agy_status or pass a shorter wait for the same job. Streams progress notifications while waiting when the " +
			"client asks for them. Read state for execution and cloud_sync_state for sync. " +
			"If either phase is pending when wait expires, follow the same job_id. include_result false omits terminal text and leaves it uncollected for agy_get_result." + inlineWaitGuide,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait, err := parseWait(in.Wait)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		// An unknown job needs no separate pre-check: WaitTerminal's first action is
		// the same Status read, and it returns before any polling, so awaitJob's
		// "status read failed" path already fails a typo'd id fast. The pre-check only
		// duplicated a potentially large out-file read on every wait against a
		// terminal job.
		out, err := awaitJob(ctx, req, mgr, in.JobID, time.Now().Add(wait), in.IncludeResult)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		return nil, out, nil
	})
}
