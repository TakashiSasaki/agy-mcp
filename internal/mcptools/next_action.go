package mcptools

import "github.com/tphakala/agy-mcp/v2/internal/manager"

const (
	actionChangeRequest      = "change_request"
	actionInspectBeforeRetry = "inspect_before_retry"
)

func statusNextAction(st manager.Status) string {
	if st.State == manager.StateRunning {
		return "wait"
	}
	if st.Partial && st.Result != "" {
		return "inspect_partial_result"
	}
	if st.State == manager.StateDone {
		return "use_result"
	}
	if st.SignInRequired {
		return "fix_auth"
	}
	if st.Retryable != nil && !*st.Retryable {
		// A quota category can also mean exhausted credits, with no known reset.
		// Keep the CLI's non-retryable verdict visible in the suggested action.
		if st.FailureReason == manager.ReasonQuotaExhausted {
			return actionInspectBeforeRetry
		}
		return actionChangeRequest
	}
	if st.FailureReason == manager.ReasonQuotaExhausted {
		return "wait_for_quota_reset"
	}
	return actionInspectBeforeRetry
}
