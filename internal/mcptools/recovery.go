package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	nextActionGetResult = "get_result"
	toolAgyListJobs     = "agy_list_jobs"
	toolAgyGetResult    = "agy_get_result"
	defaultResultBytes  = 16 * 1024
	maxResultBytes      = 1024 * 1024
)

type jobsInput struct {
	Cwd            string `json:"cwd,omitempty" jsonschema:"filter by canonical working directory; omit to search every stored job"`
	TaskLabel      string `json:"task_label,omitempty" jsonschema:"case-insensitive substring of the caller-supplied task label"`
	State          string `json:"state,omitempty" jsonschema:"filter by running, done, failed or cancelled; omit for every state"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"exact retry token used for creation; the token is never returned"`
	Limit          int    `json:"limit,omitempty" jsonschema:"page size; default 20, maximum 100"`
	Offset         int    `json:"offset,omitempty" jsonschema:"non-negative offset; pages may shift when jobs start or expire"`
}

type resultInput struct {
	JobID    string `json:"job_id" jsonschema:"job handle from agy_run, agy_run_sync or agy_list_jobs"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"opaque next_cursor from a previous page of this exact job and result; omit for the first page"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum UTF-8 bytes of result text per page; default 16384, minimum 4, maximum 1048576; metadata is additional"`
}

type resultOutput struct {
	JobID           string `json:"job_id" jsonschema:"job handle that owns this result"`
	ConversationID  string `json:"conversation_id,omitempty" jsonschema:"conversation to inspect or continue only when recovery and user authorization permit"`
	State           string `json:"state" jsonschema:"running, done, failed or cancelled; complete refers to pagination, not job success"`
	Partial         bool   `json:"partial" jsonschema:"true when the result contains only a partial answer; inspect it before continuing the conversation"`
	FailureReason   string `json:"failure_reason,omitempty" jsonschema:"the existing failure_reason category when the job failed"`
	Error           string `json:"error,omitempty" jsonschema:"the job error, if present; an error here does not make this retrieval an MCP tool error"`
	Recovery        string `json:"recovery,omitempty" jsonschema:"job-specific recovery advice; do not infer permission to retry from pagination"`
	Retryable       *bool  `json:"retryable,omitempty" jsonschema:"whether agy reported a failure can be retried; absent when it made no statement"`
	ResultAvailable bool   `json:"result_available" jsonschema:"whether this terminal snapshot contains non-empty result text"`
	Content         string `json:"content" jsonschema:"this page of result text; invalid UTF-8 is replaced with the replacement character"`
	TotalBytes      int    `json:"total_bytes" jsonschema:"byte length of the complete UTF-8 result, before pagination"`
	ResultSHA256    string `json:"result_sha256,omitempty" jsonschema:"SHA-256 of the complete UTF-8 result; absent while the job is running"`
	NextCursor      string `json:"next_cursor,omitempty" jsonschema:"cursor for the next page; rejects a different job or changed result provenance"`
	Complete        bool   `json:"complete" jsonschema:"true when no more pages remain in this terminal snapshot; does not imply successful or complete task execution"`
	NextAction      string `json:"next_action" jsonschema:"get_result when another page remains, wait while running, or advice derived from the terminal job"`
}

type resultCursor struct {
	Version int    `json:"v"`
	JobID   string `json:"job"`
	Digest  string `json:"sha256"`
	State   string `json:"state"`
	Partial bool   `json:"partial"`
	Offset  int    `json:"offset"`
}

func resultPage(st manager.Status, in resultInput) (resultOutput, error) {
	out := resultOutput{
		JobID: st.JobID, ConversationID: st.ConversationID, State: st.State, Partial: st.Partial,
		FailureReason: st.FailureReason, Error: st.Error, Recovery: toStatusOutput(st).Recovery,
		Retryable: st.Retryable, NextAction: statusNextAction(st),
	}
	limit := in.MaxBytes
	if limit == 0 {
		limit = defaultResultBytes
	}
	if limit < utf8.UTFMax || limit > maxResultBytes {
		return out, fmt.Errorf("max_bytes must be between 4 and %d", maxResultBytes)
	}
	if st.State == manager.StateRunning {
		if in.Cursor != "" {
			return out, fmt.Errorf("result cursor no longer refers to a terminal job snapshot")
		}
		return out, nil
	}
	text := strings.ToValidUTF8(st.Result, "\uFFFD")
	digest := sha256.Sum256([]byte(text))
	out.ResultSHA256 = hex.EncodeToString(digest[:])
	out.TotalBytes, out.ResultAvailable = len(text), text != ""
	snapshot := resultCursor{Version: 1, JobID: st.JobID, Digest: out.ResultSHA256, State: st.State, Partial: st.Partial}
	start := 0
	if in.Cursor != "" {
		if len(in.Cursor) > 4096 {
			return out, fmt.Errorf("invalid result cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		var cursor resultCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil {
			return out, fmt.Errorf("invalid result cursor")
		}
		start = cursor.Offset
		cursor.Offset = 0
		if cursor != snapshot || start < 0 || start > len(text) || (start < len(text) && !utf8.RuneStart(text[start])) {
			return out, fmt.Errorf("result cursor does not match this job and result snapshot; start again without a cursor")
		}
	}
	end := len(text)
	if len(text)-start > limit {
		end = start + limit
		for !utf8.RuneStart(text[end]) {
			end--
		}
	}
	out.Content, out.Complete = text[start:end], end == len(text)
	if !out.Complete {
		snapshot.Offset = end
		data, _ := json.Marshal(snapshot)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(data)
		out.NextAction = nextActionGetResult
	}
	return out, nil
}

func registerRecovery(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name: toolAgyListJobs, Title: "Find stored agy jobs", Annotations: annReadLocal,
		Description: "Recover a lost job_id without starting work. Search retained jobs by cwd, task_label, state or idempotency_key; no prompts, arguments, result bodies or retry tokens are returned. " +
			"An exact key search can return a readable match despite an unreadable job record; a miss with an incomplete scan returns an error. " +
			"Results are newest first. Offset pages may shift as jobs start or expire; total_count is an observation, not a snapshot guarantee. " +
			"Listing never marks a job collected. Use agy_status with include_result false for metadata, agy_wait to wait on the same job, or agy_get_result to read its terminal result.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in jobsInput) (*mcp.CallToolResult, manager.JobPage, error) {
		out, err := mgr.ListJobs(manager.JobQuery{Cwd: in.Cwd, TaskLabel: in.TaskLabel, State: in.State, IdempotencyKey: in.IdempotencyKey, Limit: in.Limit, Offset: in.Offset})
		return nil, out, err
	})
	addTool(s, &mcp.Tool{
		Name: toolAgyGetResult, Title: "Read an agy result page", Annotations: annReadLocal,
		Description: "Read a bounded UTF-8 page of a stored terminal result. Use agy_status with include_result false or agy_list_jobs to locate it; use agy_wait while it is running. " +
			"A cursor is bound to this job, result hash, state and partial flag; changed snapshots are rejected. complete describes pagination only: check state, partial, failure_reason and retryable before using the answer. " +
			"The final page marks a terminal job collected; intermediate pages do not. The manager still reads the stored result to derive status; max_bytes bounds returned text, not disk I/O. No agent turn is started.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in resultInput) (*mcp.CallToolResult, resultOutput, error) {
		st, err := mgr.Status(in.JobID)
		if err != nil {
			return nil, resultOutput{}, err
		}
		out, err := resultPage(st, in)
		if err == nil && out.Complete {
			markCollected(mgr, in.JobID)
		}
		return nil, out, err
	})
}
