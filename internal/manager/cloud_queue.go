package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
)

type cloudJob struct {
	meta    jobstore.Meta
	dir     string
	receipt jobstore.CloudSync
	ended   time.Time
}

type cloudLocalConversation struct {
	project, trajectoryProject string
	revision                   jobstore.CloudRevision
}

func (m *Manager) cloudLocalConversation(ctx context.Context, id string, cli daemonConversation, pinned jobstore.CloudRevision) (cloudLocalConversation, error) {
	var local cloudLocalConversation
	if !adoptionConversationID.MatchString(id) {
		return local, fmt.Errorf("invalid conversation identifier")
	}
	db, err := sql.Open("sqlite", conversationDBURI(m.conversationsDBPath, "ro"))
	if err != nil {
		return local, err
	}
	defer func() { _ = db.Close() }()
	var modified sql.NullString
	var summarySteps int
	err = db.QueryRowContext(ctx, `SELECT COALESCE(project_id, ''), step_count, last_modified_time
		FROM conversation_summaries WHERE conversation_id = ?`, id).Scan(&local.project, &summarySteps, &modified)
	if err != nil {
		return local, err
	}
	trajectoryPath := filepath.Join(filepath.Dir(m.conversationsDBPath), "conversations", id+".db")
	if err := checkAdoptionDB(trajectoryPath); err != nil {
		return local, err
	}
	trajectory, err := sql.Open("sqlite", conversationDBURI(trajectoryPath, "ro"))
	if err != nil {
		return local, err
	}
	defer func() { _ = trajectory.Close() }()
	var steps int
	var metadata []byte
	err = trajectory.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM steps), data
		FROM trajectory_metadata_blob WHERE id = 'main'`).Scan(&steps, &metadata)
	if err != nil {
		return local, err
	}
	fields, err := protohelper.ParseFields(metadata)
	if err != nil {
		return local, err
	}
	for _, field := range fields {
		if field.FieldNum == trajectoryProjectField && field.WireType == 2 {
			local.trajectoryProject = string(field.LengthBytes)
		}
	}
	summaryTime, _ := parseSQLiteTimestamp(modified.String)
	if steps < pinned.Steps {
		return local, fmt.Errorf("trajectory is older than the revision recorded for the job")
	}
	local.revision.Steps = steps
	for _, candidate := range []jobstore.CloudRevision{
		{Modified: summaryTime, Steps: summarySteps},
		{Modified: cli.revision.modified, Steps: cli.revision.steps},
		pinned,
	} {
		if candidate.Steps == steps && candidate.Modified.After(local.revision.Modified) {
			local.revision.Modified = candidate.Modified
		}
	}
	if local.revision.Modified.IsZero() || local.revision.Modified.Before(pinned.Modified) {
		return local, fmt.Errorf("summary revision does not cover the persisted trajectory (%d steps)", steps)
	}
	return local, nil
}

func (m *Manager) cloudContinuation(ctx context.Context, id string, intent *jobstore.CloudIntent) error {
	busy, err := m.cloudConversationPending(id)
	if err != nil {
		return refusedStart("cloud_sync_unavailable", "check_configuration", true, err)
	}
	if busy {
		return refusedStart("conversation_busy", "wait_for_conversation", true, fmt.Errorf("conversation %s has a job awaiting cloud sync", id))
	}
	cli, _ := readConversationSummaries(filepath.Join(filepath.Dir(m.conversationsDBPath), cliSummariesFile))
	local, err := m.cloudLocalConversation(ctx, id, cli[id], jobstore.CloudRevision{})
	if err != nil {
		return refusedStart("cloud_conversation_unavailable", "inspect_conversation", true, err)
	}
	if err := m.checkCloudProjectMove(local, intent.ProjectID); err != nil {
		return refusedStart("cloud_project_mismatch", "change_request", false, err)
	}
	intent.Baseline = local.revision
	return nil
}

func (m *Manager) checkCloudProjectMove(local cloudLocalConversation, want string) error {
	for _, current := range []string{local.project, local.trajectoryProject} {
		if current != "" && current != want {
			if project, err := m.resolveProjectTarget("", current); err == nil {
				return fmt.Errorf("conversation already belongs to cloud project %s", project.ID)
			}
		}
	}
	return nil
}

func (m *Manager) cloudConversationPending(id string) (bool, error) {
	ids, err := m.store.List()
	if err != nil {
		return false, err
	}
	for _, jobID := range ids {
		meta, err := m.store.Load(jobID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if meta.Cloud == nil || m.preparedOnly(meta) {
			continue
		}
		dir, err := m.store.Dir(jobID)
		if err != nil {
			return false, err
		}
		receipt, _ := jobstore.ReadCloudSync(dir)
		if receipt.Settled() {
			continue
		}
		progress, _ := jobstore.ReadProgressDir(dir)
		if id == meta.ConversationID || id == progress.ConversationID || id == receipt.ConversationID {
			return true, nil
		}
		if meta.ConversationID == "" && progress.ConversationID == "" && !m.processAlive(meta) {
			st, err := m.Status(jobID)
			if err != nil {
				return false, err
			}
			if st.ConversationID == id {
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *Manager) cloudJobs() (jobs []cloudJob, active bool, err error) {
	ids, err := m.store.List()
	if err != nil {
		return nil, false, err
	}
	for _, id := range ids {
		meta, err := m.store.Load(id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("read job %s before daemon maintenance: %w", id, err)
		}
		if m.processAlive(meta) {
			active = true
			continue
		}
		if meta.Cloud == nil || m.preparedOnly(meta) {
			continue
		}
		dir, err := m.store.Dir(id)
		if err != nil {
			return nil, false, err
		}
		receipt, _ := jobstore.ReadCloudSync(dir)
		if !receipt.Settled() && !receipt.RetryAt.After(m.now()) {
			_, exitMarker := m.store.ExitCode(id)
			ended, _ := m.retentionStart(meta, exitMarker, true)
			jobs = append(jobs, cloudJob{meta: meta, dir: dir, receipt: receipt, ended: ended})
		}
	}
	return jobs, active, nil
}

func (j *cloudJob) write(state, reason string, now time.Time) error {
	j.receipt.State, j.receipt.Reason = state, reason
	switch state {
	case jobstore.CloudUnavailable:
		j.receipt.Attempts = min(j.receipt.Attempts+1, 6)
		j.receipt.RetryAt = now.Add(time.Second << j.receipt.Attempts)
	case jobstore.CloudConfirmed, jobstore.CloudFailed:
		j.receipt.SettledAt = now.UTC()
		j.receipt.RetryAt = time.Time{}
	default:
		j.receipt.RetryAt = time.Time{}
	}
	return jobstore.WriteCloudSync(j.dir, j.receipt)
}

func (j *cloudJob) unavailable(now time.Time, reason string, err error) error {
	log.Printf("cloud sync job %s conversation %s: %s: %v", j.meta.ID, j.receipt.ConversationID, reason, err)
	return j.write(jobstore.CloudUnavailable, reason, now)
}

func (j cloudJob) target() daemonConversation {
	return daemonConversation{
		project:  j.meta.Cloud.ProjectID,
		title:    j.receipt.Title,
		revision: conversationRevision{modified: j.receipt.Revision.Modified, steps: j.receipt.Revision.Steps},
	}
}

func cloudMatches(entry, target daemonConversation) bool {
	return !target.revision.modified.IsZero() && entry.revision.steps >= target.revision.steps && entry.matches(target)
}

// stageCloudJob persists the job's exact target before a daemon shutdown can
// replay an older local summary. Only a durable receipt enters a batch.
func (m *Manager) stageCloudJob(ctx context.Context, j *cloudJob, cli map[string]daemonConversation) (bool, error) {
	now := m.now()
	status, err := m.Status(j.meta.ID)
	if err != nil {
		return false, j.unavailable(now, "job_status_unavailable", err)
	}
	if status.State == StateRunning {
		return false, nil
	}
	id := status.ConversationID
	if !adoptionConversationID.MatchString(id) {
		return false, j.write(jobstore.CloudFailed, "conversation_missing", now)
	}
	progress, _ := jobstore.ReadProgressDir(j.dir)
	if (progress.ConversationID != "" && progress.ConversationID != id) ||
		(j.receipt.ConversationID != "" && j.receipt.ConversationID != id) {
		return false, j.write(jobstore.CloudFailed, "conversation_id_mismatch", now)
	}
	j.receipt.ConversationID = id
	local, err := m.cloudLocalConversation(ctx, id, cli[id], j.receipt.Revision)
	if err != nil {
		return false, j.unavailable(now, "conversation_revision_unavailable", err)
	}
	baseline := j.meta.Cloud.Baseline
	if status.State == StateDone && !baseline.Modified.IsZero() &&
		(!local.revision.Modified.After(baseline.Modified) || local.revision.Steps <= baseline.Steps) {
		return false, j.unavailable(now, "new_revision_pending", fmt.Errorf("continuation has not published a newer trajectory"))
	}
	if err := m.checkCloudProjectMove(local, j.meta.Cloud.ProjectID); err != nil {
		return false, j.unavailable(now, "conversation_project_changed", err)
	}
	title := j.meta.Cloud.Title
	if j.receipt.Title != "" {
		title = j.receipt.Title
	}
	res, err := m.persistAdoption(ctx, m.conversationsDBPath,
		AdoptConversationInput{ConversationID: id, Project: j.meta.Cloud.ProjectID, Title: title})
	if err != nil {
		return false, j.unavailable(now, "local_adoption_unavailable", err)
	}
	j.receipt.Title, j.receipt.Revision = res.Title, local.revision
	return true, j.write(jobstore.CloudPending, "awaiting_daemon_revision", now)
}
