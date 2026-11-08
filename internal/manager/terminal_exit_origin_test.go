package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestTerminalExitOrigin(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		code                  int
		signal                *bool
		timedOut, cancelled   bool
		wantState, wantReason string
	}{
		{name: "ordinary 124", code: 124, wantState: StateFailed, wantReason: ReasonAgyError},
		{name: "ordinary 127", code: 127, wantState: StateFailed, wantReason: ReasonAgyError},
		{name: "ordinary 130", code: 130, signal: new(false), wantState: StateFailed, wantReason: ReasonAgyError},
		{name: "ordinary 143", code: 143, signal: new(false), wantState: StateFailed, wantReason: ReasonAgyError},
		{name: "legacy 143", code: 143, wantState: StateCancelled},
		{name: "SIGINT", code: 130, signal: new(true), wantState: StateCancelled},
		{name: "SIGTERM", code: 143, signal: new(true), wantState: StateCancelled},
		{name: "deadline", code: 143, signal: new(true), timedOut: true, wantState: StateFailed, wantReason: ReasonTimeout},
		{name: "cancel after handler exit", code: 1, signal: new(false), cancelled: true, wantState: StateCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			dir := createJob(t, m, "exit-origin")
			start := time.Now().Add(-time.Second)
			if err := jobstore.WriteProcessDir(dir, jobstore.ProcessOutcome{
				Source: jobstore.ProcessOutcomeSource, PID: 42, StartedAt: &start, EndedAt: time.Now(), ExitCode: &tc.code,
				SignalTermination: tc.signal, TimedOut: tc.timedOut, Cancelled: tc.cancelled,
			}); err != nil {
				t.Fatal(err)
			}
			if err := jobstore.WriteResultDir(dir, []byte(`{"status":"SUCCESS","response":"saved result"}`)); err != nil {
				t.Fatal(err)
			}
			for _, withMarker := range []bool{false, true} {
				if withMarker {
					if err := m.store.WriteExitCode("exit-origin", jobstore.ExitSentinel(tc.code, tc.timedOut, tc.cancelled)); err != nil {
						t.Fatal(err)
					}
				}
				st, err := m.Status("exit-origin")
				if err != nil || st.State != tc.wantState || st.FailureReason != tc.wantReason || st.Result != "saved result" || st.Partial {
					t.Fatalf("with marker %t: status=%+v error=%v, want %s/%s and exact terminal result", withMarker, st, err, tc.wantState, tc.wantReason)
				}
				if tc.wantReason == ReasonAgyError && !strings.HasPrefix(st.Error, "exit ") {
					t.Fatalf("ordinary exit has wrong diagnostic: %q", st.Error)
				}
				if state, err := m.State("exit-origin"); err != nil || state != tc.wantState {
					t.Fatalf("with marker %t: state=%q error=%v, want %q", withMarker, state, err, tc.wantState)
				}
			}
		})
	}
}
