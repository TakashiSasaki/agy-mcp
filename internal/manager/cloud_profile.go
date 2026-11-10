package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

const (
	cloudLinux           = "linux"
	cloudAGYVersion      = "1.5.3"
	cloudProfileDir      = ".agy-mcp-sync"
	cloudWorkerFile      = "worker.json"
	cloudStoreFile       = "store.json"
	cloudJournalFile     = "maintenance.json"
	cloudGateName        = "admission"
	cloudWorkerName      = "worker"
	remoteControlService = "antigravity-cli-daemon.service"
)

// ErrCloudAGYVersion indicates that the worker found an unqualified CLI
// version. Retrying startup cannot help until the installation changes.
var ErrCloudAGYVersion = errors.New("unsupported AGY version for cloud job sync")

type cloudWorkerIdentity struct {
	PID      int       `json:"pid"`
	BootID   string    `json:"boot_id"`
	Ticks    uint64    `json:"start_ticks"`
	Store    string    `json:"store"`
	AGYPath  string    `json:"agy_path"`
	AGYSize  int64     `json:"agy_size"`
	AGYMTime time.Time `json:"agy_mtime"`
}

type cloudMaintenance struct {
	NeedsStart  bool      `json:"needs_start"`
	LastRestart time.Time `json:"last_restart,omitzero"`
}

func (m *Manager) cloudDir() string {
	return filepath.Join(filepath.Dir(m.conversationsDBPath), cloudProfileDir)
}

func (m *Manager) tryCloudLock(name string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(m.cloudDir(), name+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := flockExclusiveNB(f.Fd())
	if !held || err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readCloudJSON[T any](path string) (T, error) {
	var v T
	f, info, err := jobstore.OpenRegularFile(path)
	if err != nil {
		return v, err
	}
	defer func() { _ = f.Close() }()
	const limit = 16 << 10
	if info.Size() > limit {
		return v, fmt.Errorf("oversized cloud state file %s", filepath.Base(path))
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return v, err
	}
	if len(b) > limit || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return v, fmt.Errorf("invalid cloud state file %s", filepath.Base(path))
	}
	err = json.Unmarshal(b, &v)
	return v, err
}

func (m *Manager) writeCloudJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return jobstore.WritePrivateFileAtomic(m.cloudDir(), name, data)
}

func (m *Manager) cloudJournal() (cloudMaintenance, error) {
	v, err := readCloudJSON[cloudMaintenance](filepath.Join(m.cloudDir(), cloudJournalFile))
	if errors.Is(err, os.ErrNotExist) {
		return cloudMaintenance{}, nil
	}
	return v, err
}

func (m *Manager) verifyCloudWorker(agy string) error {
	w, err := readCloudJSON[cloudWorkerIdentity](filepath.Join(m.cloudDir(), cloudWorkerFile))
	if err != nil {
		return fmt.Errorf("sync worker readiness: %w", err)
	}
	store, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return err
	}
	if store != w.Store {
		return fmt.Errorf("sync worker uses another job store")
	}
	if ticks, ok := readStartTimeTicks(w.PID); !ok || w.BootID != readBootID() || ticks != w.Ticks {
		return fmt.Errorf("sync worker process has exited")
	}
	exe, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	workerExe, err := os.Stat(fmt.Sprintf("/proc/%d/exe", w.PID))
	if err != nil || !os.SameFile(exe, workerExe) {
		return fmt.Errorf("sync worker and MCP server must run the same installed binary")
	}
	// A stale identity file cannot substitute for the worker's lifetime lock.
	lease, err := m.tryCloudLock(cloudWorkerName)
	if err != nil {
		return err
	}
	if lease != nil {
		_ = lease.Close()
		return fmt.Errorf("sync worker does not hold its profile lock")
	}
	return w.checkAGY(agy)
}

func (w cloudWorkerIdentity) checkAGY(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if w.AGYPath != resolved || w.AGYSize != info.Size() || !w.AGYMTime.Equal(info.ModTime()) {
		return fmt.Errorf("AGY binary changed since sync worker startup")
	}
	return nil
}

func (m *Manager) identifyCloudWorker(ctx context.Context) (cloudWorkerIdentity, error) {
	var identity cloudWorkerIdentity
	if runtime.GOOS != cloudLinux {
		return identity, fmt.Errorf("cloud job sync requires Linux and the systemd user service")
	}
	if err := checkAdoptionDB(m.conversationsDBPath); err != nil {
		return identity, err
	}
	store, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return identity, err
	}
	agy, err := m.cfg.AgyBinary()
	if err != nil {
		return identity, err
	}
	agy, err = filepath.EvalSymlinks(agy)
	if err != nil {
		return identity, err
	}
	info, err := os.Stat(agy)
	if err != nil {
		return identity, err
	}
	if err := m.checkCloudAGYVersion(ctx, agy); err != nil {
		return identity, err
	}
	pid := os.Getpid()
	ticks, ok := readStartTimeTicks(pid)
	if !ok || readBootID() == "" {
		return identity, fmt.Errorf("cannot verify sync worker process identity")
	}
	return cloudWorkerIdentity{PID: pid, BootID: readBootID(), Ticks: ticks, Store: store,
		AGYPath: agy, AGYSize: info.Size(), AGYMTime: info.ModTime()}, nil
}

func (m *Manager) checkCloudAGYVersion(ctx context.Context, agy string) error {
	raw, err := m.readAgyVersion(ctx, agy)
	if err != nil {
		return err
	}
	version, err := agyver.Parse(raw)
	if err != nil {
		return fmt.Errorf("cannot verify AGY version for cloud job sync (requires %s): %w", cloudAGYVersion, err)
	}
	if version.String() != cloudAGYVersion {
		return fmt.Errorf("%w: found %s, requires qualified AGY %s", ErrCloudAGYVersion, version, cloudAGYVersion)
	}
	return nil
}

// StartCloudJob is the MCP job admission path. StartJob also serves internal
// callers that exercise the supervisor independently of a live AGY profile.
func (m *Manager) StartCloudJob(req StartRequest) (Job, error) {
	if strings.TrimSpace(req.Project) == "" {
		return Job{}, refusedStart("invalid_argument", "change_request", false, fmt.Errorf("project is required for cloud job sync"))
	}
	return m.StartJob(req)
}

func (m *Manager) prepareJobLaunch(req StartRequest) (agy string, intent *jobstore.CloudIntent, release func(), err error) {
	agy, err = m.agyBinaryChecked(context.Background())
	if err != nil {
		return "", nil, nil, refusedStart("agy_unavailable", "check_configuration", false, err)
	}
	if req.Project == "" {
		return agy, nil, func() {}, nil
	}
	gate, intent, err := m.cloudAdmission(req, agy)
	if err != nil {
		return "", nil, nil, err
	}
	// Held through durable supervisor admission, including the pipe close.
	return agy, intent, func() { _ = gate.Close() }, nil
}

func startedJob(id, conversationID string, cloud *jobstore.CloudIntent) Job {
	job := Job{ID: id, ConversationID: conversationID, State: StateRunning}
	if cloud != nil {
		job.CloudProjectID, job.CloudSyncState = cloud.ProjectID, jobstore.CloudPending
	}
	return job
}

func (m *Manager) cloudAdmission(req StartRequest, agy string) (gate *os.File, intent *jobstore.CloudIntent, err error) {
	if runtime.GOOS != cloudLinux {
		return nil, nil, refusedStart("cloud_sync_unsupported", "change_environment", false,
			fmt.Errorf("cloud job sync requires a qualified Linux systemd deployment"))
	}
	if len(req.Title) > 4096 {
		return nil, nil, refusedStart("invalid_argument", "change_request", false, fmt.Errorf("title exceeds 4096 bytes"))
	}
	project, err := m.resolveProjectTarget(req.Project, "")
	if err != nil {
		return nil, nil, refusedStart("cloud_project_invalid", "change_request", false, err)
	}
	gate, err = m.tryCloudLock(cloudGateName)
	if err != nil {
		return nil, nil, refusedStart("cloud_sync_unavailable", "check_configuration", true, fmt.Errorf("open cloud admission gate: %w", err))
	}
	if gate == nil {
		return nil, nil, refusedStart("cloud_sync_busy", "retry_same_request", true, fmt.Errorf("cloud sync or another admission is using the profile"))
	}
	// Keep the file in this local so named return values cannot lose it on error.
	lock := gate
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	if err := m.verifyCloudWorker(agy); err != nil {
		return nil, nil, refusedStart("cloud_sync_unavailable", "check_configuration", true, err)
	}
	journal, err := m.cloudJournal()
	if err != nil || journal.NeedsStart {
		return nil, nil, refusedStart("cloud_sync_busy", "retry_same_request", true,
			fmt.Errorf("maintenance recovery for Remote Control is pending"))
	}
	intent = &jobstore.CloudIntent{ProjectID: project.ID, Title: req.Title}
	if req.ConversationID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.cloudContinuation(ctx, req.ConversationID, intent); err != nil {
			return nil, nil, err
		}
	}
	return gate, intent, nil
}

// cloudManualGate coordinates manual tools with the worker when this profile
// has been configured. Keep standalone repair available before installation.
func (m *Manager) cloudManualGate() (release func(), managed bool, err error) {
	if runtime.GOOS != cloudLinux {
		return func() {}, false, nil
	}
	gate, err := m.tryCloudLock(cloudGateName)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if gate == nil {
		return nil, false, fmt.Errorf("cloud sync maintenance or job admission is in progress")
	}
	journal, err := m.cloudJournal()
	if err != nil || journal.NeedsStart {
		_ = gate.Close()
		return nil, false, fmt.Errorf("maintenance recovery for Remote Control is pending")
	}
	store, err := readCloudJSON[string](filepath.Join(m.cloudDir(), cloudStoreFile))
	if err == nil {
		configured, resolveErr := filepath.EvalSymlinks(m.cfg.StateDir)
		if resolveErr != nil || configured != store {
			_ = gate.Close()
			return nil, false, fmt.Errorf("cloud maintenance must use the profile's shared job store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = gate.Close()
		return nil, false, err
	}
	return func() { _ = gate.Close() }, true, nil
}
