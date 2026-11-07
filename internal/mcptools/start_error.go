package mcptools

import (
	"errors"

	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const startInvalidArgument = "invalid_argument"

type startErrorOutput struct {
	StartErrorCode string `json:"start_error_code" jsonschema:"original agy-mcp start refusal code, equal to native error_code; use this field when a connector replaces error_code with a generic transport category"`
	ErrorCode      string `json:"error_code" jsonschema:"machine-readable reason this run request failed; separate from a job's failure_reason"`
	Message        string `json:"message" jsonschema:"the original actionable error message"`
	JobID          string `json:"job_id,omitempty" jsonschema:"known job to inspect; absent when no job handle is known"`
	JobStarted     *bool  `json:"job_started" jsonschema:"false when this request started no new work; true when its job was created; null when work may already have started, including a retry key being claimed"`
	Retryable      bool   `json:"retryable" jsonschema:"whether the same request may be retried under the recommended action; never override user authorization"`
	NextAction     string `json:"next_action" jsonschema:"advisory recovery action, such as change_request, skip_optional, wait_for_capacity, retry_same_request or inspect_before_retry"`
	RetryAfter     string `json:"retry_after,omitempty" jsonschema:"RFC 3339 quota reset time when supplied; absent when a retry time is unknown"`
	QuotaGroup     string `json:"quota_group,omitempty" jsonschema:"quota group that refused this optional request"`
}

func describeStartError(err error) startErrorOutput {
	out := startErrorOutput{ErrorCode: "start_failed", Message: err.Error(), NextAction: actionInspectBeforeRetry}
	if start, ok := errors.AsType[*manager.StartError](err); ok {
		out.ErrorCode, out.NextAction, out.JobStarted = start.Code, start.NextAction, start.JobStarted
		out.JobID, out.Retryable = start.JobID, start.Retryable
	}
	if quota, ok := errors.AsType[*manager.QuotaRefusalError](err); ok {
		out.ErrorCode, out.NextAction, out.JobStarted = "quota_low", "skip_optional", new(false)
		out.QuotaGroup, out.RetryAfter = quota.Group, formatQuotaTime(quota.ResetTime)
		if quota.MatchStatus != manager.QuotaMatchExact {
			out.ErrorCode, out.NextAction = "quota_mapping_unknown", "select_model"
		}
	}
	out.StartErrorCode = out.ErrorCode
	return out
}

func invalidRunError(err error) error {
	return &manager.StartError{Code: startInvalidArgument, NextAction: actionChangeRequest, JobStarted: new(false), Err: err}
}
