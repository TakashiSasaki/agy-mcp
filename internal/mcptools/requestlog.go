package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

var requestLogSequence atomic.Uint64

const verificationOutcomeUnknown = "unknown"

// Log only server-generated job IDs, never arbitrary client input rejected by
// a handler. The store creates a nanosecond timestamp and 12 random hex bytes.
var loggedJobID = regexp.MustCompile(`^\d{1,20}-[0-9a-f]{24}$`)

type requestLogEntry struct {
	Event                  string `json:"event"`
	Phase                  string `json:"phase"`
	PID                    int    `json:"pid"`
	CallID                 uint64 `json:"call_id"`
	Method                 string `json:"method"`
	Tool                   string `json:"tool,omitempty"`
	ElapsedMS              *int64 `json:"elapsed_ms,omitempty"`
	Outcome                string `json:"outcome,omitempty"`
	JobID                  string `json:"job_id,omitempty"`
	JobState               string `json:"job_state,omitempty"`
	FailureReason          string `json:"failure_reason,omitempty"`
	Partial                bool   `json:"partial,omitempty"`
	StartErrorCode         string `json:"start_error_code,omitempty"`
	JobStarted             *bool  `json:"job_started,omitempty"`
	ResultComplete         *bool  `json:"result_complete,omitempty"`
	VerificationOutcome    string `json:"verification_outcome,omitempty"`
	CloudSyncState         string `json:"cloud_sync_state,omitempty"`
	DaemonCacheInvalidated *bool  `json:"daemon_cache_invalidated,omitempty"`
	CLICacheInvalidated    *bool  `json:"cli_cache_invalidated,omitempty"`
	DaemonRestarted        *bool  `json:"daemon_restarted,omitempty"`
	ToolCount              *int   `json:"tool_count,omitempty"`
	CatalogSHA256          string `json:"catalog_sha256,omitempty"`
	CatalogComplete        *bool  `json:"catalog_complete,omitempty"`
}

// requestLoggingMiddleware records handler progress, not transport delivery.
// A successful MCP call may carry a failed job; retain both outcomes. Never log
// arguments, headers, content, error text, or arbitrary structured result data.
func requestLoggingMiddleware(logger *log.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			switch method {
			case "initialize", "server/discover", "tools/list", "tools/call":
			default:
				return next(ctx, method, req)
			}
			entry := requestLogEntry{
				Event: "mcp_request", Phase: "started", PID: os.Getpid(),
				CallID: requestLogSequence.Add(1), Method: method,
			}
			firstCatalogPage := true
			if list, ok := req.(*mcp.ListToolsRequest); ok && list != nil && list.Params != nil {
				firstCatalogPage = list.Params.Cursor == ""
			}
			if call, ok := req.(*mcp.CallToolRequest); ok && call != nil && call.Params != nil {
				entry.Tool = loggedToolName(call.Params.Name)
				var input struct {
					JobID string `json:"job_id"`
				}
				if json.Unmarshal(call.Params.Arguments, &input) == nil && loggedJobID.MatchString(input.JobID) {
					entry.JobID = input.JobID
				}
			}
			writeRequestLog(logger, entry)
			started := time.Now()
			finished := false
			defer func() {
				elapsed := time.Since(started).Milliseconds()
				entry.Phase, entry.ElapsedMS, entry.Outcome = "completed", &elapsed, "ok"
				switch {
				case !finished:
					entry.Outcome = "panic"
				case errors.Is(err, context.Canceled):
					entry.Outcome = "cancelled"
				case errors.Is(err, context.DeadlineExceeded):
					entry.Outcome = "deadline_exceeded"
				case err != nil:
					entry.Outcome = "protocol_error"
				default:
					entry.addResult(result)
					if entry.CatalogComplete != nil && !firstCatalogPage {
						entry.CatalogComplete = new(false)
					}
				}
				writeRequestLog(logger, entry)
			}()
			result, err = next(ctx, method, req)
			finished = true
			return result, err
		}
	}
}

func writeRequestLog(logger *log.Logger, entry requestLogEntry) {
	// The record contains only strings, booleans and integers, so encoding cannot
	// fail. Use the standard stderr logger; stdout belongs to the MCP transport.
	data, _ := json.Marshal(entry)
	logger.Print(string(data))
}

func loggedToolName(name string) string {
	switch name {
	case toolAgyRun, toolAgyRunSync, toolAgyStatus, toolAgyWait, toolAgyCancel,
		toolAgyUsage, toolListModels, toolListAgents, toolListSessions,
		toolAgyListArtifacts, toolAgyReadArtifact, toolAgyListProjects, toolAgyResolveWorkspace,
		toolAgyListJobs, toolAgyGetResult, toolAgyInfo, toolAgyEvents, toolAgyLogs, toolAgyVerification,
		toolAgyListConversations, toolAgyGetConversation, toolAgyAdoptConversation, toolAgyRestartSyncDaemon:
		return name
	default:
		return "unknown"
	}
}

func (entry *requestLogEntry) addResult(result mcp.Result) {
	switch result := result.(type) {
	case *mcp.CallToolResult:
		if result == nil {
			return
		}
		if result.IsError {
			entry.Outcome = "tool_error"
			if entry.Tool == toolAgyRun || entry.Tool == toolAgyRunSync {
				entry.addStartError(result.StructuredContent)
			}
			return
		}
		// go-sdk v1.8.0's typed tool adapter stores structured output as raw JSON.
		// Decode only metadata; ignored result strings do not need a second copy.
		var output struct {
			JobID                  string `json:"job_id"`
			State                  string `json:"state"`
			FailureReason          string `json:"failure_reason"`
			Partial                bool   `json:"partial"`
			Complete               *bool  `json:"complete"`
			VerificationOutcome    string `json:"verification_outcome"`
			CloudSyncState         string `json:"cloud_sync_state"`
			DaemonCacheInvalidated *bool  `json:"daemon_cache_invalidated"`
			CLICacheInvalidated    *bool  `json:"cli_cache_invalidated"`
			DaemonRestarted        *bool  `json:"daemon_restarted"`
		}
		data, ok := result.StructuredContent.(json.RawMessage)
		if !ok || json.Unmarshal(data, &output) != nil {
			return
		}
		if loggedJobID.MatchString(output.JobID) {
			entry.JobID = output.JobID
		}
		switch output.State {
		case manager.StateRunning, manager.StateDone, manager.StateFailed, manager.StateCancelled:
			entry.JobState, entry.Partial = output.State, output.Partial
		}
		if entry.JobState == manager.StateFailed {
			switch output.FailureReason {
			case manager.ReasonQuotaExhausted, manager.ReasonTimeout, manager.ReasonSpawnFailed,
				manager.ReasonAgyError, manager.ReasonInterrupted, manager.ReasonBackgroundAborted, manager.ReasonUnknown:
				entry.FailureReason = output.FailureReason
			default:
				entry.FailureReason = manager.ReasonUnknown
			}
		}
		switch entry.Tool {
		case toolAgyGetResult:
			entry.ResultComplete = output.Complete
		case toolAgyVerification:
			switch output.VerificationOutcome {
			case "passed", "failed", verificationOutcomeUnknown:
				entry.VerificationOutcome = output.VerificationOutcome
			}
		case toolAgyAdoptConversation:
			switch output.CloudSyncState {
			case manager.ConversationSyncConfirmed, manager.ConversationSyncPending, manager.ConversationSyncUnavailable:
				entry.CloudSyncState = output.CloudSyncState
			}
			entry.DaemonRestarted = output.DaemonRestarted
			entry.DaemonCacheInvalidated = output.DaemonCacheInvalidated
			entry.CLICacheInvalidated = output.CLICacheInvalidated
		case toolAgyRestartSyncDaemon:
			entry.DaemonRestarted = output.DaemonRestarted
		}
	case *mcp.ListToolsResult:
		if result == nil {
			return
		}
		count, complete := len(result.Tools), result.NextCursor == ""
		entry.ToolCount, entry.CatalogComplete = &count, &complete
		entry.CatalogSHA256 = catalogDigest(result.Tools)
	}
}

func (entry *requestLogEntry) addStartError(structured any) {
	var output struct {
		Code       string `json:"start_error_code"`
		JobID      string `json:"job_id"`
		JobStarted *bool  `json:"job_started"`
	}
	data, ok := structured.(json.RawMessage)
	if !ok || json.Unmarshal(data, &output) != nil {
		return
	}
	switch output.Code {
	case startInvalidArgument, "workspace_unavailable", "workspace_mismatch",
		"conversation_busy", "conversation_cache_unavailable", "capacity_exceeded",
		"idempotency_conflict", "idempotency_in_progress", "agy_unavailable",
		"quota_low", "quota_mapping_unknown", "unsupported_platform",
		"start_failed", "start_indeterminate", "wait_interrupted":
		entry.StartErrorCode, entry.JobStarted = output.Code, output.JobStarted
		if loggedJobID.MatchString(output.JobID) {
			entry.JobID = output.JobID
		}
	}
}

// Canonicalize through JSON values so schema struct field ordering and decoded
// client maps produce the same digest. Array order inside schemas is retained.
func catalogDigest(catalog []*mcp.Tool) string {
	tools := slices.Clone(catalog)
	slices.SortFunc(tools, func(a, b *mcp.Tool) int {
		if a == nil {
			if b == nil {
				return 0
			}
			return -1
		}
		if b == nil {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	data, err := json.Marshal(tools)
	if err != nil {
		return ""
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return ""
	}
	data, _ = json.Marshal(decoded)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
