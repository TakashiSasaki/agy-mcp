package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

const (
	cloudPollInterval = 2 * time.Second
	cloudDrainAfter   = 15 * time.Second
	cloudCooldown     = 15 * time.Second
)

// RunCloudSyncWorker reconciles the shared job store independently of MCP
// connections. The profile lease allows one worker even if another job store
// is accidentally pointed at the same AGY profile.
func (m *Manager) RunCloudSyncWorker(ctx context.Context) error {
	if err := jobstore.MkdirAllDurable(m.cfg.StateDir, 0o700); err != nil {
		return err
	}
	identity, err := m.identifyCloudWorker(ctx)
	if err != nil {
		return err
	}
	if err := jobstore.MkdirAllDurable(m.cloudDir(), 0o700); err != nil {
		return err
	}
	lease, err := m.tryCloudLock(cloudWorkerName)
	if err != nil {
		return err
	}
	if lease == nil {
		return fmt.Errorf("another cloud sync worker owns this AGY profile")
	}
	defer func() { _ = lease.Close() }()
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := probeCmd(check, "systemctl", "--user", "show", "-p", "LoadState", "--value", remoteControlService).Output()
	cancel()
	if err != nil || strings.TrimSpace(string(state)) != "loaded" {
		return fmt.Errorf("cloud sync requires the loaded %s user unit", remoteControlService)
	}
	store, err := readCloudJSON[string](filepath.Join(m.cloudDir(), cloudStoreFile))
	if errors.Is(err, os.ErrNotExist) {
		err = m.writeCloudJSON(cloudStoreFile, identity.Store)
	} else if err == nil && store != identity.Store {
		return fmt.Errorf("AGY profile is already associated with job store %s", store)
	}
	if err != nil {
		return err
	}
	if err := m.writeCloudJSON(cloudWorkerFile, identity); err != nil {
		return err
	}
	defer func() { _ = os.Remove(filepath.Join(m.cloudDir(), cloudWorkerFile)) }()
	log.Printf("cloud sync worker ready for %s on AGY %s", identity.Store, cloudAGYVersion)

	ticker := time.NewTicker(cloudPollInterval)
	defer ticker.Stop()
	var gate *os.File
	defer func() {
		if gate != nil {
			_ = gate.Close()
		}
	}()
	for ctx.Err() == nil {
		if err := identity.checkAGY(identity.AGYPath); err != nil {
			return err
		}
		if gate == nil {
			gate, err = m.tryCloudLock(cloudGateName)
			if err != nil {
				return err
			}
		}
		if gate != nil {
			drain, err := m.cloudRound(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("cloud sync worker: %v", err)
			}
			if !drain || err != nil {
				_ = gate.Close()
				gate = nil
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil
}

func (m *Manager) startCloudDaemon(ctx context.Context) error {
	if m.cloudStartDaemon != nil {
		return m.cloudStartDaemon(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return probeCmd(ctx, "systemctl", "--user", "start", remoteControlService).Run()
}

func (m *Manager) cloudRound(ctx context.Context) (drain bool, err error) {
	journal, err := m.cloudJournal()
	if err != nil {
		return false, err
	}
	if journal.NeedsStart {
		if err := m.startCloudDaemon(ctx); err != nil {
			return false, fmt.Errorf("recover Remote Control after incomplete maintenance: %w", err)
		}
		log.Print("cloud sync recovered Remote Control after incomplete maintenance")
		return false, m.writeCloudJSON(cloudJournalFile, cloudMaintenance{LastRestart: m.now().UTC()})
	}

	jobs, active, err := m.cloudJobs()
	if err != nil || len(jobs) == 0 {
		return false, err
	}
	if active {
		for i := range jobs {
			if !jobs[i].ended.IsZero() && m.now().Sub(jobs[i].ended) >= cloudDrainAfter {
				return true, nil
			}
		}
		return false, nil
	}
	// The profile admission lock remains held from this check through restart.
	// SQLite covers AGY activity outside the managed supervisors.
	idleCheck, cancel := context.WithTimeout(ctx, 3*time.Second)
	activeConversations, err := countActiveConversations(idleCheck, m.conversationsDBPath)
	cancel()
	if err != nil || activeConversations != 0 {
		return false, err
	}

	snapshotDir := filepath.Dir(m.conversationsDBPath)
	cli, cliErr := readConversationSummaries(filepath.Join(snapshotDir, cliSummariesFile))
	daemon, daemonErr := readDaemonConversations(m.conversationsDBPath)
	if err := errors.Join(summaryReadError(cliErr), summaryReadError(daemonErr)); err != nil {
		for i := range jobs {
			if writeErr := jobs[i].unavailable(m.now(), "summary_snapshot_unavailable", err); writeErr != nil {
				return false, writeErr
			}
		}
		return false, nil
	}

	batch := make([]cloudJob, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		stage, cancel := context.WithTimeout(ctx, 5*time.Second)
		ready, err := m.stageCloudJob(stage, job, cli)
		cancel()
		if err != nil {
			return false, err
		}
		if !ready {
			continue
		}
		if cloudSnapshotsMatch(*job, cli, daemon) {
			if err := m.confirmCloudJob(job); err != nil {
				return false, err
			}
		} else {
			batch = append(batch, *job)
		}
	}
	if len(batch) == 0 || m.now().Sub(journal.LastRestart) < cloudCooldown {
		return false, nil
	}
	// Check again immediately before stopping, after the potentially slow
	// SQLite and snapshot preparation. Never force an automatic restart.
	idleCheck, cancel = context.WithTimeout(ctx, 3*time.Second)
	activeConversations, err = countActiveConversations(idleCheck, m.conversationsDBPath)
	cancel()
	if err != nil || activeConversations != 0 {
		return false, err
	}
	return false, m.refreshCloudBatch(ctx, batch, journal)
}

func summaryReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cloudSnapshotsMatch(job cloudJob, cli, daemon map[string]daemonConversation) bool {
	id, target := job.receipt.ConversationID, job.target()
	cached, exists := cli[id]
	return cloudMatches(daemon[id], target) && (!exists || cloudMatches(cached, target))
}

func (m *Manager) confirmCloudJob(job *cloudJob) error {
	if err := job.write(jobstore.CloudConfirmed, "", m.now()); err != nil {
		return err
	}
	log.Printf("cloud sync confirmed job %s conversation %s steps=%d", job.meta.ID,
		job.receipt.ConversationID, job.receipt.Revision.Steps)
	return nil
}

func (m *Manager) refreshCloudBatch(ctx context.Context, batch []cloudJob, journal cloudMaintenance) error {
	journal.NeedsStart = true
	if err := m.writeCloudJSON(cloudJournalFile, journal); err != nil {
		return err
	}
	restart := m.restartDaemonCmd
	if restart == nil {
		restart = restartRemoteControlDaemon
	}
	refresh, cancel := context.WithTimeout(ctx, 30*time.Second)
	started, err := restart(refresh, func(prepare context.Context) error {
		return m.prepareCloudBatch(prepare, batch)
	})
	cancel()
	if started {
		if writeErr := m.writeCloudJSON(cloudJournalFile, cloudMaintenance{LastRestart: m.now().UTC()}); writeErr != nil {
			return errors.Join(err, writeErr)
		}
		log.Printf("cloud sync restarted Remote Control for %d conversation(s)", len(batch))
	}
	if err != nil || !started {
		for i := range batch {
			if writeErr := batch[i].unavailable(m.now(), "daemon_refresh_unavailable", err); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		return err
	}
	return m.verifyCloudBatch(ctx, batch)
}

func (m *Manager) prepareCloudBatch(ctx context.Context, batch []cloudJob) error {
	targets := make(map[string]daemonConversation, len(batch))
	for i := range batch {
		job := &batch[i]
		id, target := job.receipt.ConversationID, job.target()
		if _, err := m.persistAdoption(ctx, m.conversationsDBPath,
			AdoptConversationInput{ConversationID: id, Project: target.project, Title: target.title}); err != nil {
			return err
		}
		targets[id] = target
	}
	// The daemon and all MCP-launched AGY writers are stopped. Preserve all
	// unrelated entries and keep the summary file identity checks.
	for _, file := range []string{cliSummariesFile, daemonSummariesFile} {
		if _, err := invalidateSummaryEntries(filepath.Join(filepath.Dir(m.conversationsDBPath), file),
			func(id string, summary daemonConversation) (bool, bool) {
				target, selected := targets[id]
				return selected, selected && !cloudMatches(summary, target)
			}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) verifyCloudBatch(ctx context.Context, batch []cloudJob) error {
	wait, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for len(batch) != 0 {
		daemon, daemonErr := readDaemonConversations(m.conversationsDBPath)
		cli, cliErr := readConversationSummaries(filepath.Join(filepath.Dir(m.conversationsDBPath), cliSummariesFile))
		if daemonErr == nil && summaryReadError(cliErr) == nil {
			pending := batch[:0]
			for i := range batch {
				job := &batch[i]
				if cloudSnapshotsMatch(*job, cli, daemon) {
					if err := m.confirmCloudJob(job); err != nil {
						return err
					}
				} else {
					pending = append(pending, *job)
				}
			}
			batch = pending
		}
		if len(batch) == 0 {
			return nil
		}
		select {
		case <-wait.Done():
			for i := range batch {
				if err := batch[i].unavailable(m.now(), "daemon_revision_unavailable", wait.Err()); err != nil {
					return err
				}
			}
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

// GuardCloudWatchdog runs the existing host watchdog only while the profile
// is idle. The lock also prevents it from starting the daemon during refresh.
func (m *Manager) GuardCloudWatchdog(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("watchdog command is required")
	}
	gate, err := m.tryCloudLock(cloudGateName)
	if err != nil {
		return err
	}
	if gate == nil {
		return nil
	}
	defer func() { _ = gate.Close() }()
	journal, err := m.cloudJournal()
	if err != nil || journal.NeedsStart {
		return err
	}
	_, active, err := m.cloudJobs()
	if err != nil || active {
		return err
	}
	activeConversations, err := countActiveConversations(ctx, m.conversationsDBPath)
	if err != nil || activeConversations != 0 {
		return err
	}
	return probeCmd(ctx, args[0], args[1:]...).Run()
}
