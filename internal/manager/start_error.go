package manager

// StartError preserves the original error while exposing a recovery decision.
// JobStarted is nil when a supervisor may already have executed work. A caller
// must inspect that work before retrying, even if failed-start cleanup removes
// the job directory later. These codes are separate from job failure reasons.
type StartError struct {
	Code       string
	NextAction string
	JobID      string
	JobStarted *bool
	Retryable  bool
	Err        error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

func refusedStart(code, action string, retryable bool, err error) error {
	return &StartError{Code: code, NextAction: action, JobStarted: new(false), Retryable: retryable, Err: err}
}

func indeterminateStart(id string, err error) error {
	return &StartError{Code: "start_indeterminate", NextAction: "inspect_before_retry", JobID: id, Err: err}
}
