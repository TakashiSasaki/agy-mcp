package manager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// Mutation guards: label validation in normalizeRequest, TaskLabel exclusion
// from requestKey/buildAgyArgs, and task_label in the persisted Meta.
func TestAgentTaskLabelIsValidatedMetadata(t *testing.T) {
	m := newManager(t, managerOpts{defaultTimeout: time.Minute})
	base := StartRequest{Prompt: "private prompt", Cwd: t.TempDir(), IdempotencyKey: "retry-token"}
	first, err := m.normalizeRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.TaskLabel = "  回帰テスト  "
	labelled, err := m.normalizeRequest(base)
	if err != nil || labelled.TaskLabel != "回帰テスト" {
		t.Fatalf("label normalization: %+v, %v", labelled, err)
	}
	if requestKey(first) != requestKey(labelled) || !slices.Equal(buildAgyArgs(first), buildAgyArgs(labelled)) {
		t.Fatal("a metadata label changed execution identity or CLI arguments")
	}
	for _, invalid := range []string{strings.Repeat("界", 129), "label\x00", "line\nbreak", string([]byte{0xff})} {
		base.TaskLabel = invalid
		if _, err := m.normalizeRequest(base); err == nil {
			t.Errorf("accepted invalid label %q", invalid)
		}
	}
	base.TaskLabel = strings.Repeat("界", 128)
	if _, err := m.normalizeRequest(base); err != nil {
		t.Errorf("128-rune label refused: %v", err)
	}
	if _, err := m.store.Create(jobstore.Meta{ID: "existing", PID: -1, TaskLabel: "original", Cwd: first.Cwd, IdempotencyKey: first.IdempotencyKey, RequestKey: requestKey(first)}); err != nil {
		t.Fatal(err)
	}
	job, found, err := m.findIdempotentJob(labelled, buildAgyArgs(labelled))
	if err != nil || !found || job.ID != "existing" {
		t.Fatalf("changed label did not replay: %+v, %t, %v", job, found, err)
	}
	meta, err := m.store.Load(job.ID)
	if err != nil || meta.TaskLabel != "original" {
		t.Fatalf("replay changed the stored label: %+v, %v", meta, err)
	}
}

// Mutation guards: typed refusals in StartJob/findIdempotentJob and the nil
// JobStarted on indeterminateStart. None of these cases launches a supervisor.
func TestAgentStartErrorDecisions(t *testing.T) {
	m := newManager(t, managerOpts{agyPath: filepath.Join(t.TempDir(), "agy"), defaultTimeout: time.Minute, maxConcurrency: 1})
	_, err := m.StartJob(StartRequest{ConversationID: "conversation", ContinueLatest: true})
	start, ok := errors.AsType[*StartError](err)
	if !ok || start.Code != "invalid_argument" || start.JobStarted == nil || *start.JobStarted || start.Retryable {
		t.Fatalf("invalid request decision: %#v, %v", start, err)
	}
	cause := errors.New("injected after spawn")
	uncertain := indeterminateStart("known-job", cause)
	start, ok = errors.AsType[*StartError](uncertain)
	if !errors.Is(uncertain, cause) || !ok || start.JobStarted != nil || start.JobID != "known-job" || start.Retryable || start.NextAction != "inspect_before_retry" {
		t.Fatalf("uncertain start must preserve possible execution: %+v", start)
	}
	if !proc.Supported {
		return
	}
	m.gate.inFlight = 1
	_, err = m.StartJob(StartRequest{Prompt: "capacity", Cwd: t.TempDir()})
	start, ok = errors.AsType[*StartError](err)
	if !ok || start.Code != "capacity_exceeded" || start.JobStarted == nil || *start.JobStarted || !start.Retryable || start.NextAction != "wait_for_capacity" {
		t.Fatalf("capacity decision: %+v, %v", start, err)
	}
	m.gate.inFlight = 0
	req, err := m.normalizeRequest(StartRequest{Prompt: "one", Cwd: t.TempDir(), IdempotencyKey: "same-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Create(jobstore.Meta{ID: "existing", PID: 1, Cwd: req.Cwd, IdempotencyKey: req.IdempotencyKey, RequestKey: requestKey(req)}); err != nil {
		t.Fatal(err)
	}
	req.Prompt = "changed"
	_, err = m.StartJob(req)
	start, ok = errors.AsType[*StartError](err)
	if !ok || start.Code != "idempotency_conflict" || start.JobStarted == nil || *start.JobStarted || start.Retryable {
		t.Fatalf("conflicting retry decision: %+v, %v", start, err)
	}
}

func stageRecoveryJobs(t *testing.T) (*Manager, string, time.Time) {
	t.Helper()
	m := newManager(t, managerOpts{})
	cwd, err := normalizeCwd(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		meta := jobstore.Meta{ID: id, Cwd: cwd, TaskLabel: "Review API", Prompt: "PRIVATE-PROMPT", Args: []string{"PRIVATE-ARG"}, IdempotencyKey: "PRIVATE-KEY", StartedAt: started.Add(time.Duration(i/2) * time.Minute)}
		if id == "c" {
			meta.TaskLabel, meta.Cwd, meta.IdempotencyKey = "Documentation", t.TempDir(), "OTHER-KEY"
		}
		dir, err := m.store.Create(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jobstore.OutPath(dir), []byte("PRIVATE-RESULT"), 0o600); err != nil {
			t.Fatal(err)
		}
		code := 0
		if id == "b" {
			code = jobstore.ExitTimeout
		}
		if err := m.store.WriteExitCode(id, code); err != nil {
			t.Fatal(err)
		}
	}
	return m, cwd, started
}

// Mutation guards: ListJobs' filters, sorting, pagination and public projection;
// Status' job/label/progress copies. Real terminal files exercise classification.
func TestAgentJobRecovery(t *testing.T) {
	m, cwd, _ := stageRecoveryJobs(t)
	t.Run("ordering and pagination", func(t *testing.T) {
		page, err := m.ListJobs(JobQuery{Limit: 2})
		if err != nil || page.TotalCount != 3 || len(page.Jobs) != 2 || page.Jobs[0].JobID != "c" || page.Jobs[1].JobID != "b" || page.NextOffset == nil || *page.NextOffset != 2 {
			t.Fatalf("newest/tie/page ordering: %+v, %v", page, err)
		}
		tail, err := m.ListJobs(JobQuery{Limit: 2, Offset: 2})
		if err != nil || len(tail.Jobs) != 1 || tail.Jobs[0].JobID != "a" || tail.NextOffset != nil {
			t.Fatalf("tail page: %+v, %v", tail, err)
		}
	})
	t.Run("filters and privacy", func(t *testing.T) {
		for _, query := range []JobQuery{{Cwd: cwd}, {TaskLabel: "REVIEW"}, {IdempotencyKey: "PRIVATE-KEY"}} {
			page, err := m.ListJobs(query)
			if err != nil || len(page.Jobs) != 2 || page.Jobs[0].JobID != "b" {
				t.Fatalf("filter %+v: %+v, %v", query, page, err)
			}
		}
		page, err := m.ListJobs(JobQuery{State: StateFailed})
		if err != nil || len(page.Jobs) != 1 || page.Jobs[0].JobID != "b" || page.Jobs[0].FailureReason != ReasonTimeout {
			t.Fatalf("state classification filter: %+v, %v", page, err)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"PRIVATE-PROMPT", "PRIVATE-ARG", "PRIVATE-KEY", "PRIVATE-RESULT"} {
			if strings.Contains(string(encoded), private) {
				t.Errorf("listing exposed %s", private)
			}
		}
		if m.Collected("b") {
			t.Fatal("listing collected a result")
		}
	})
}

// Mutation guard: an exact-key lookup can recover its readable match even
// when an unrelated record could not be scanned.
func TestAgentJobRecoveryWithUnreadableNeighbor(t *testing.T) {
	m, _, _ := stageRecoveryJobs(t)
	dir, err := m.store.Create(jobstore.Meta{ID: "unreadable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.MetaPath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := m.ListJobs(JobQuery{IdempotencyKey: "PRIVATE-KEY"})
	if err != nil || page.TotalCount != 2 || len(page.Jobs) != 2 || page.Jobs[0].JobID != "b" {
		t.Fatalf("known key lost despite readable matches: %+v, %v", page, err)
	}
	for _, q := range []JobQuery{{IdempotencyKey: "missing"}, {}} {
		if _, err := m.ListJobs(q); err == nil {
			t.Fatalf("incomplete lookup returned a definitive listing for %+v", q)
		}
	}
}

// Mutation guards: ListJobs' query bounds and Status' progress projection.
func TestAgentJobRecoveryBoundsAndProgress(t *testing.T) {
	m, _, started := stageRecoveryJobs(t)
	t.Run("invalid and out of range queries", func(t *testing.T) {
		for _, query := range []JobQuery{{Limit: -1}, {Limit: 101}, {Offset: -1}, {State: "invented"}} {
			if _, err := m.ListJobs(query); err == nil {
				t.Errorf("accepted invalid query %+v", query)
			}
		}
		empty, err := m.ListJobs(JobQuery{Offset: int(^uint(0) >> 1)})
		if err != nil || len(empty.Jobs) != 0 || empty.NextOffset != nil {
			t.Fatalf("offset beyond store: %+v, %v", empty, err)
		}
	})
	t.Run("status identity and progress", func(t *testing.T) {
		store := jobstore.New(m.cfg.StateDir)
		progress := jobstore.Progress{UpdatedAt: started, StepIndex: 7, StepType: "tool"}
		dir, err := store.Dir("b")
		if err != nil {
			t.Fatal(err)
		}
		if err := jobstore.WriteProgressDir(dir, progress); err != nil {
			t.Fatal(err)
		}
		st, err := m.Status("b")
		if err != nil || st.JobID != "b" || st.TaskLabel != "Review API" || !st.ProgressUpdatedAt.Equal(started) || st.StepIndex != 7 {
			t.Fatalf("status identity/progress: %+v, %v", st, err)
		}
	})
}
