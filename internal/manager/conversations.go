package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func defaultConversationsDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")
}

// ConversationSummaryItem models a single conversation session summary.
type ConversationSummaryItem struct {
	ConversationID   string   `json:"conversation_id" jsonschema:"unique UUID of the conversation session"`
	Title            string   `json:"title" jsonschema:"human-readable title of the conversation"`
	ProjectID        string   `json:"project_id" jsonschema:"associated project ID"`
	ProjectName      string   `json:"project_name,omitempty" jsonschema:"human-readable project name"`
	IsCloudSynced    bool     `json:"is_cloud_synced" jsonschema:"true when the Remote Control daemon summary confirms the project, title, and latest observed local revision"`
	CloudSyncState   string   `json:"cloud_sync_state" jsonschema:"confirmed by the Remote Control daemon summary, pending, unavailable, or local_only"`
	SyncScope        string   `json:"sync_scope,omitempty" jsonschema:"cloud sync scope: 'cloud', 'local_only', 'cli_default', or 'unregistered'"`
	Archived         bool     `json:"archived" jsonschema:"true if the conversation has been archived"`
	Status           string   `json:"status" jsonschema:"execution lifecycle status of the conversation"`
	StepCount        int      `json:"step_count" jsonschema:"number of execution steps recorded in the session"`
	WorkspaceURIs    []string `json:"workspace_uris" jsonschema:"associated workspace folder URIs"`
	LastModifiedTime string   `json:"last_modified_time" jsonschema:"RFC3339 timestamp when the conversation was last updated"`
}

// ListConversationsInput provides parameters to filter and paginate conversation sessions.
type ListConversationsInput struct {
	ConversationID  string `json:"conversation_id,omitempty" jsonschema:"optional exact conversation ID (UUID) to filter"`
	ProjectID       string `json:"project_id,omitempty" jsonschema:"optional project ID or UUID to filter conversations (e.g. 'c5f32b28-...', 'default-cli-project', 'outside-of-project')"`
	Query           string `json:"query,omitempty" jsonschema:"optional substring to search within conversation titles"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"whether to include archived conversations (default: false)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum number of conversations to return (default: 50, maximum: 200)"`
	Offset          int    `json:"offset,omitempty" jsonschema:"pagination offset (default: 0)"`
}

// ListConversationsResult carries matching conversation summaries and pagination bounds.
type ListConversationsResult struct {
	Conversations []ConversationSummaryItem `json:"conversations"`
	TotalCount    int                       `json:"total_count"`
	HasMore       bool                      `json:"has_more"`
}

// AdoptConversationInput provides parameters for adopting a conversation into a project.
type AdoptConversationInput struct {
	ConversationID string `json:"conversation_id" jsonschema:"required: true, conversation ID (UUID) to adopt into a cloud-synced project"`
	Project        string `json:"project,omitempty" jsonschema:"target project ID, name, or directory path. Required if the conversation has no current cloud project"`
	Title          string `json:"title,omitempty" jsonschema:"optional title to assign; if omitted, retains existing non-empty title or derives from user prompt preview"`
	RestartDaemon  *bool  `json:"restart_daemon,omitempty" jsonschema:"whether to attempt a graceful restart of antigravity-cli-daemon if idle (default: false)"`
	ForceRestart   bool   `json:"force_restart,omitempty" jsonschema:"if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

// AdoptConversationResult returns the outcome of conversation adoption and verification.
type AdoptConversationResult struct {
	Success                bool   `json:"success" jsonschema:"true if local project and title persistence succeeded"`
	AlreadyConverted       bool   `json:"already_converted" jsonschema:"true when the local summary, trajectory metadata, and annotation already have the requested binding and title"`
	ConversationID         string `json:"conversation_id" jsonschema:"conversation ID that was processed"`
	ProjectID              string `json:"project_id" jsonschema:"resolved project UUID"`
	ProjectName            string `json:"project_name" jsonschema:"human-readable name of the target project"`
	Title                  string `json:"title" jsonschema:"title assigned or retained on the conversation"`
	MetadataUpdated        bool   `json:"metadata_updated" jsonschema:"true if the local SQLite summary was updated"`
	TrajectoryBlobUpdated  bool   `json:"trajectory_blob_updated" jsonschema:"true if conversations/<id>.db trajectory_metadata_blob was updated"`
	AnnotationsUpdated     bool   `json:"annotations_updated" jsonschema:"true if annotations/<id>.pbtxt was updated"`
	RPCNotified            bool   `json:"rpc_notified" jsonschema:"true if the configured language server accepted a title update in the same project"`
	CloudSyncState         string `json:"cloud_sync_state" jsonschema:"confirmed by the Remote Control daemon summary, pending, or unavailable"`
	DaemonCacheInvalidated bool   `json:"daemon_cache_invalidated" jsonschema:"true if a stale summary for this conversation was removed while the daemon was stopped"`
	CLICacheInvalidated    bool   `json:"cli_cache_invalidated" jsonschema:"true if the stale CLI summary for this conversation was removed"`
	DaemonRestarted        bool   `json:"daemon_restarted" jsonschema:"true if antigravity-cli-daemon was restarted"`
	DaemonRestartReason    string `json:"daemon_restart_reason,omitempty" jsonschema:"explanation of restart outcome"`
	ActiveSessionsCount    int    `json:"active_sessions_count,omitempty" jsonschema:"number of currently active/running sessions detected if restart was skipped"`
	PostRestartVerified    bool   `json:"post_restart_verified" jsonschema:"true if this call restarted the daemon and observed its updated summary"`
	revision               conversationRevision
}

// DaemonRestartResult carries the output of a standalone graceful sync daemon restart.
type DaemonRestartResult struct {
	Success             bool   `json:"success" jsonschema:"true if restart completed or was cleanly skipped due to active sessions"`
	DaemonRestarted     bool   `json:"daemon_restarted" jsonschema:"true if antigravity-cli-daemon was restarted"`
	DaemonRestartReason string `json:"daemon_restart_reason" jsonschema:"explanation of restart outcome"`
	ActiveSessionsCount int    `json:"active_sessions_count" jsonschema:"number of currently active sessions detected"`
}

type projectConversationStats struct {
	Count      int
	LastActive string
}

func parseSQLiteTimestamp(raw string) (modified time.Time, formatted string) {
	if raw == "" || strings.HasPrefix(raw, "0001-01-01") {
		return time.Time{}, ""
	}
	formats := []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t, t.UTC().Format(time.RFC3339)
		}
	}
	return time.Time{}, raw
}

// GetProjectConversationStats inspects conversation_summaries.db and computes
// the number of sessions and latest activity timestamp grouped by project_id.
func (m *Manager) GetProjectConversationStats() (map[string]projectConversationStats, error) {
	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return make(map[string]projectConversationStats), nil
	}

	db, err := sql.Open("sqlite", conversationDBURI(dbPath, "ro"))
	if err != nil {
		return nil, fmt.Errorf("open conversation db read-only: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query("SELECT project_id, count(*), max(last_modified_time) FROM conversation_summaries GROUP BY project_id")
	if err != nil {
		return nil, fmt.Errorf("query project stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	stats := make(map[string]projectConversationStats)
	for rows.Next() {
		var pid sql.NullString
		var count int
		var maxTime sql.NullString
		if err := rows.Scan(&pid, &count, &maxTime); err != nil {
			continue
		}
		key := pid.String
		if key == "" {
			key = "unassigned"
		}
		_, rfc := parseSQLiteTimestamp(maxTime.String)
		stats[key] = projectConversationStats{
			Count:      count,
			LastActive: rfc,
		}
	}
	return stats, nil
}

// ListConversations queries conversation_summaries.db for matching conversation records.
func (m *Manager) ListConversations(in ListConversationsInput) (*ListConversationsResult, error) {
	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("conversation database not found at %s: %w", dbPath, err)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	offset := max(in.Offset, 0)

	projects, err := m.ListProjects("")
	if err != nil {
		projects = []ProjectInfo{}
	}
	projectMap := make(map[string]ProjectInfo)
	for i := range projects {
		projectMap[projects[i].ID] = projects[i]
	}

	db, err := sql.Open("sqlite", conversationDBURI(dbPath, "ro"))
	if err != nil {
		return nil, fmt.Errorf("open conversation db read-only: %w", err)
	}
	defer func() { _ = db.Close() }()

	var hasFTS bool
	var ftsCheck int
	_ = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'conversations_fts'").Scan(&ftsCheck)
	hasFTS = ftsCheck > 0

	var whereClauses []string
	var args []any

	if strings.TrimSpace(in.ConversationID) != "" {
		whereClauses = append(whereClauses, "c.conversation_id = ?")
		args = append(args, strings.TrimSpace(in.ConversationID))
	}

	if in.ProjectID != "" {
		whereClauses = append(whereClauses, "c.project_id = ?")
		args = append(args, in.ProjectID)
	}
	if in.Query != "" {
		whereClauses = append(whereClauses, "c.title LIKE ?")
		args = append(args, "%"+in.Query+"%")
	}
	if !in.IncludeArchived && hasFTS {
		whereClauses = append(whereClauses, "(f.archived IS NULL OR f.archived = 0)")
	}

	var fromClause string
	if hasFTS {
		fromClause = "FROM conversation_summaries c LEFT JOIN (SELECT conversation_id, MAX(archived) as archived FROM conversations_fts GROUP BY conversation_id) f ON c.conversation_id = f.conversation_id"
	} else {
		fromClause = "FROM conversation_summaries c"
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	countQuery := "SELECT count(*) " + fromClause + whereSQL
	var totalCount int
	if err := db.QueryRow(countQuery, args...).Scan(&totalCount); err != nil {
		return nil, fmt.Errorf("count conversations: %w", err)
	}

	selectCols := "c.conversation_id, c.title, c.step_count, c.last_modified_time, c.workspace_uris, c.status, c.project_id"
	if hasFTS {
		selectCols += ", COALESCE(f.archived, 0)"
	} else {
		selectCols += ", 0"
	}

	dataQuery := fmt.Sprintf("SELECT %s %s%s ORDER BY c.last_modified_time DESC LIMIT ? OFFSET ?", selectCols, fromClause, whereSQL)
	args = append(args, limit, offset)

	rows, err := db.Query(dataQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []ConversationSummaryItem
	daemonSummaries, _ := readDaemonConversations(dbPath)
	cliSummaries, _ := readConversationSummaries(filepath.Join(filepath.Dir(dbPath), cliSummariesFile))
	for rows.Next() {
		var (
			cid          string
			title        sql.NullString
			stepCount    int
			lastModRaw   sql.NullString
			wsRaw        sql.NullString
			status       sql.NullString
			projectID    sql.NullString
			archivedFlag int
		)
		if err := rows.Scan(&cid, &title, &stepCount, &lastModRaw, &wsRaw, &status, &projectID, &archivedFlag); err != nil {
			return nil, fmt.Errorf("scan conversation row: %w", err)
		}

		var wsList []string
		if wsRaw.Valid && wsRaw.String != "" {
			_ = json.Unmarshal([]byte(wsRaw.String), &wsList)
		}
		if wsList == nil {
			wsList = []string{}
		}

		modified, lastModRFC := parseSQLiteTimestamp(lastModRaw.String)

		syncState := ConversationSyncLocalOnly
		syncScope := "unregistered"
		projName := ""
		if proj, ok := projectMap[projectID.String]; ok {
			projName = proj.Name
			syncScope = proj.SyncScope
			if proj.IsCloudSynced && strings.TrimSpace(title.String) != "" {
				revision := conversationRevision{modified: modified, steps: stepCount}
				target := daemonConversation{project: projectID.String, title: title.String, revision: revision.latest(cliSummaries[cid].revision)}
				syncState = daemonConversationState(daemonSummaries, cid, target)
			}
		} else if projectID.String == "default-cli-project" {
			syncScope = "cli_default"
		}

		items = append(items, ConversationSummaryItem{
			ConversationID:   cid,
			Title:            title.String,
			ProjectID:        projectID.String,
			ProjectName:      projName,
			IsCloudSynced:    syncState == ConversationSyncConfirmed,
			CloudSyncState:   syncState,
			SyncScope:        syncScope,
			Archived:         archivedFlag > 0,
			Status:           status.String,
			StepCount:        stepCount,
			WorkspaceURIs:    wsList,
			LastModifiedTime: lastModRFC,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read conversations: %w", err)
	}

	if items == nil {
		items = []ConversationSummaryItem{}
	}

	return &ListConversationsResult{
		Conversations: items,
		TotalCount:    totalCount,
		HasMore:       offset+len(items) < totalCount,
	}, nil
}

// GetConversation retrieves detailed metadata and cloud-sync status for a single conversation.
func (m *Manager) GetConversation(conversationID string) (*ConversationSummaryItem, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, errors.New("conversation_id is required")
	}
	res, err := m.ListConversations(ListConversationsInput{
		ConversationID:  conversationID,
		IncludeArchived: true,
		Limit:           1,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Conversations) == 0 {
		return nil, fmt.Errorf("conversation %q not found", conversationID)
	}
	return &res.Conversations[0], nil
}

// RestartSyncDaemon exposes standalone graceful restart functionality.
func (m *Manager) RestartSyncDaemon(ctx context.Context, force bool) (*DaemonRestartResult, error) {
	restarted, reason, activeCount, err := m.restartSyncDaemon(ctx, force, nil)
	if err != nil {
		return nil, err
	}
	return &DaemonRestartResult{
		Success:             true,
		DaemonRestarted:     restarted,
		DaemonRestartReason: reason,
		ActiveSessionsCount: activeCount,
	}, nil
}

// restartSyncDaemon performs an idle-checked graceful restart of antigravity-cli-daemon.
func (m *Manager) restartSyncDaemon(ctx context.Context, force bool, prepare func(context.Context) error) (restarted bool, reason string, active int, err error) {
	m.daemonRestartMu.Lock()
	defer m.daemonRestartMu.Unlock()

	const daemonKey = "daemon:remote-control-restart"
	if m.xlock != nil {
		locked, err := m.xlock.tryLock(daemonKey)
		if err != nil {
			return false, "could not lock the daemon restart", 0, err
		}
		if !locked {
			return false, "another daemon restart is in progress", 0, nil
		}
		defer m.xlock.unlock(daemonKey)
	}

	now := time.Now()
	if !force && now.Sub(m.lastDaemonRestart) < 15*time.Second {
		rem := 15*time.Second - now.Sub(m.lastDaemonRestart)
		return false, fmt.Sprintf("debounced restart: last restart was %v ago (cooldown %v remaining)", now.Sub(m.lastDaemonRestart).Round(time.Second), rem.Round(time.Second)), 0, nil
	}

	if !force {
		dbPath := m.conversationsDBPath
		if dbPath == "" {
			dbPath = defaultConversationsDBPath()
		}
		running, err := countActiveConversations(ctx, dbPath)
		if err != nil {
			return false, "could not verify the daemon is idle", 0, err
		}
		if running > 0 {
			return false, fmt.Sprintf("%d active conversation(s). Restart skipped", running), running, nil
		}
	}

	restart := m.restartDaemonCmd
	if restart == nil {
		restart = restartRemoteControlDaemon
	}

	restartCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	restarted, err = restart(restartCtx, prepare)
	if restarted {
		m.lastDaemonRestart = time.Now()
	}
	if err != nil {
		return restarted, fmt.Sprintf("daemon refresh error: %v", err), 0, err
	}

	return true, "antigravity-cli-daemon restarted gracefully", 0, nil
}

func restartRemoteControlDaemon(ctx context.Context, prepare func(context.Context) error) (started bool, err error) {
	const service = "antigravity-cli-daemon.service"
	if prepare == nil {
		err = probeCmd(ctx, "systemctl", "--user", "restart", service).Run()
		return err == nil, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancelStop()
	if err := probeCmd(stopCtx, "systemctl", "--user", "stop", service).Run(); err != nil {
		return false, fmt.Errorf("stop Remote Control daemon: %w", err)
	}
	// Restore the service even when the tool call is cancelled during preparation.
	defer func() {
		startCtx, cancelStart := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancelStart()
		startErr := probeCmd(startCtx, "systemctl", "--user", "start", service).Run()
		started = startErr == nil
		if startErr != nil {
			err = errors.Join(err, fmt.Errorf("start Remote Control daemon: %w", startErr))
		}
	}()
	return false, prepare(ctx)
}

func countActiveConversations(ctx context.Context, dbPath string) (int, error) {
	if err := checkAdoptionDB(dbPath); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", conversationDBURI(dbPath, "ro"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_summaries WHERE not_fully_idle != 0 OR status IN ('CASCADE_RUN_STATUS_RUNNING', 'RUNNING')").Scan(&count)
	return count, err
}
