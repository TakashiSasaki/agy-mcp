package manager

import (
	"os"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestStatusRecoversProcessExitCode(t *testing.T) {
	for _, tt := range []struct {
		name       string
		exit       int
		timedOut   bool
		cancelled  bool
		source     string
		sentinel   *int
		wantState  string
		wantCause  string
		streamOnly bool
	}{
		{name: "success", exit: 0, wantState: StateDone},
		{name: "success with streamed output only", exit: 0, streamOnly: true, wantState: StateDone},
		{name: "failure with streamed output only", exit: 42, streamOnly: true, wantState: StateFailed, wantCause: ReasonAgyError},
		{name: "failure despite success payload", exit: 42, wantState: StateFailed, wantCause: ReasonAgyError},
		{name: "signal crash", exit: 139, wantState: StateFailed, wantCause: ReasonAgyError},
		{name: "timeout escalated to SIGKILL", exit: 137, timedOut: true, wantState: StateFailed, wantCause: ReasonTimeout},
		{name: "cancellation escalated to SIGKILL", exit: 137, cancelled: true, wantState: StateCancelled},
		{name: "unrecognized receipt source", exit: 42, source: "external", wantState: StateDone},
		{name: "contradictory receipt flags", exit: 137, timedOut: true, cancelled: true, wantState: StateDone},
		{name: "existing sentinel wins", exit: 42, sentinel: new(0), wantState: StateDone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			start := time.Now().Add(-time.Minute)
			dir, err := m.store.Create(jobstore.Meta{
				ID: "j", StartedAt: start, PID: 999999, BootID: "old-boot", Args: []string{"--output-format", "stream-json"},
			})
			if err != nil {
				t.Fatal(err)
			}
			source := tt.source
			if source == "" {
				source = "supervisor"
			}
			if err := jobstore.WriteProcessDir(dir, jobstore.ProcessOutcome{
				Source: source, PID: 999998, StartedAt: &start, EndedAt: time.Now(), ExitCode: &tt.exit,
				TimedOut: tt.timedOut, Cancelled: tt.cancelled,
			}); err != nil {
				t.Fatal(err)
			}
			if tt.streamOnly {
				if err := os.WriteFile(jobstore.OutPath(dir), []byte("recovered answer"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := jobstore.WriteResultDir(dir, []byte(`{"status":"SUCCESS","response":"recovered answer"}`)); err != nil {
				t.Fatal(err)
			}
			if tt.sentinel != nil {
				if err := m.store.WriteExitCode("j", *tt.sentinel); err != nil {
					t.Fatal(err)
				}
			}
			st, err := m.Status("j")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.wantState || st.FailureReason != tt.wantCause || st.Result != "recovered answer" || st.Partial != tt.streamOnly {
				t.Fatalf("status = %q/%q result %q partial %t, want %q/%q and saved result with partial %t",
					st.State, st.FailureReason, st.Result, st.Partial, tt.wantState, tt.wantCause, tt.streamOnly)
			}
		})
	}
}
