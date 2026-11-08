package manager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

type JobQuery struct {
	Cwd, TaskLabel, State, IdempotencyKey string
	Limit, Offset                         int
}

// JobSummary deliberately excludes prompts, arguments, errors and retry keys.
type JobSummary struct {
	JobID             string `json:"job_id" jsonschema:"handle accepted by agy_status, agy_wait and agy_get_result"`
	TaskLabel         string `json:"task_label,omitempty" jsonschema:"caller-supplied label; never inferred from the prompt"`
	Cwd               string `json:"cwd" jsonschema:"canonical working directory recorded when the job started"`
	RequestedModel    string `json:"requested_model,omitempty" jsonschema:"requested model; not proof of the model that executed the job"`
	ConversationID    string `json:"conversation_id,omitempty" jsonschema:"conversation that can be continued when appropriate"`
	State             string `json:"state" jsonschema:"running, done, failed or cancelled"`
	FailureReason     string `json:"failure_reason,omitempty" jsonschema:"the existing job failure category, when failed"`
	StartedAt         string `json:"started_at,omitempty" jsonschema:"UTC RFC 3339 start timestamp; absent if unknown"`
	ProgressUpdatedAt string `json:"progress_updated_at,omitempty" jsonschema:"UTC RFC 3339 timestamp of the last observed progress update"`
}

type JobPage struct {
	Jobs       []JobSummary `json:"jobs" jsonschema:"matching jobs ordered by start time descending, then job ID descending"`
	TotalCount int          `json:"total_count" jsonschema:"matching jobs observed during this scan before pagination"`
	NextOffset *int         `json:"next_offset,omitempty" jsonschema:"offset for the next page when more matches were observed"`
}

func jobTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// preparedOnly identifies a metadata record written before supervisor PID
// publication. Older supervisors could execute before that publication, so
// keep any binding with evidence of execution or an indeterminate file check.
func (m *Manager) preparedOnly(meta jobstore.Meta) bool {
	if meta.PID != 0 {
		return false
	}
	dir, err := m.store.Dir(meta.ID)
	if err != nil {
		return false
	}
	for _, name := range []string{jobstore.OutFile, jobstore.ErrFile, jobstore.ProgressFile, jobstore.ProcessFile,
		jobstore.ResultFile, jobstore.ExitCodeFile, jobstore.EventsFile, jobstore.CaptureFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

func (m *Manager) matchesJobQuery(meta jobstore.Meta, cwd, label, key string) bool {
	if (cwd != "" && meta.Cwd != cwd) || (label != "" && !strings.Contains(strings.ToLower(meta.TaskLabel), label)) {
		return false
	}
	return key == "" || (meta.IdempotencyKey == key && !m.preparedOnly(meta))
}

// ListJobs never collects or cancels a job. State is derived by Status so failed
// result payloads and early-exit notices keep exactly the same interpretation.
// Concurrent creation or garbage collection can change later offset pages.
func (m *Manager) ListJobs(q JobQuery) (JobPage, error) {
	out := JobPage{Jobs: []JobSummary{}}
	if q.Limit < 0 || q.Limit > 100 || q.Offset < 0 {
		return out, fmt.Errorf("limit must be between 0 and 100, and offset must be non-negative")
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	switch q.State {
	case "", StateRunning, StateDone, StateFailed, StateCancelled:
	default:
		return out, fmt.Errorf("invalid state filter %q", q.State)
	}
	cwd, err := normalizeCwd(q.Cwd)
	if err != nil {
		return out, err
	}
	ids, err := m.store.List()
	if err != nil {
		return out, err
	}
	var matches []jobstore.Meta
	var scanErr error
	label := strings.ToLower(q.TaskLabel)
	for _, id := range ids {
		meta, err := m.store.Load(id)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			if q.IdempotencyKey == "" {
				return out, fmt.Errorf("read job %s during listing: %w", id, err)
			}
			scanErr = fmt.Errorf("read job %s during listing: %w", id, err)
			continue
		}
		if m.matchesJobQuery(meta, cwd, label, q.IdempotencyKey) {
			matches = append(matches, meta)
		}
	}
	slices.SortFunc(matches, func(a, b jobstore.Meta) int {
		if cmp := b.StartedAt.Compare(a.StartedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(b.ID, a.ID)
	})
	for i := range matches {
		meta := &matches[i]
		st, err := m.Status(meta.ID)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("read job %s status during listing: %w", meta.ID, err)
		}
		if q.State != "" && st.State != q.State {
			continue
		}
		index := out.TotalCount
		out.TotalCount++
		if index < q.Offset || len(out.Jobs) >= q.Limit {
			continue
		}
		out.Jobs = append(out.Jobs, JobSummary{
			JobID: meta.ID, TaskLabel: meta.TaskLabel, Cwd: meta.Cwd,
			RequestedModel: meta.Model, ConversationID: st.ConversationID,
			State: st.State, FailureReason: st.FailureReason,
			StartedAt: jobTime(meta.StartedAt), ProgressUpdatedAt: jobTime(st.ProgressUpdatedAt),
		})
	}
	// A readable exact-key hit can recover a lost handle. A failed scan
	// cannot establish the absence of that handle.
	if out.TotalCount == 0 && scanErr != nil {
		return out, scanErr
	}
	if q.Offset < out.TotalCount && len(out.Jobs) < out.TotalCount-q.Offset {
		out.NextOffset = new(q.Offset + len(out.Jobs))
	}
	return out, nil
}
