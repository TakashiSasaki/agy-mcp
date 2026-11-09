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
	IsCloudSynced    bool     `json:"is_cloud_synced" jsonschema:"true if the conversation belongs to a registered cloud-synced project"`
	Archived         bool     `json:"archived" jsonschema:"true if the conversation has been archived"`
	Status           string   `json:"status" jsonschema:"execution lifecycle status of the conversation"`
	StepCount        int      `json:"step_count" jsonschema:"number of execution steps recorded in the session"`
	WorkspaceURIs    []string `json:"workspace_uris" jsonschema:"associated workspace folder URIs"`
	LastModifiedTime string   `json:"last_modified_time" jsonschema:"RFC3339 timestamp when the conversation was last updated"`
}

// ListConversationsInput provides parameters to filter and paginate conversation sessions.
type ListConversationsInput struct {
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

// AdoptConversationInput provides parameters for adopting a conversation into a project
// and/or performing a graceful sync daemon restart.
type AdoptConversationInput struct {
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"optional conversation ID (UUID) to adopt into a cloud-synced project; if omitted, performs a standalone graceful sync daemon restart"`
	Project        string `json:"project,omitempty" jsonschema:"target project ID, name, or directory path to bind the conversation to (required when conversation_id is provided)"`
	Title          string `json:"title,omitempty" jsonschema:"optional title to assign; if omitted, retains existing non-empty title or derives from user prompt preview"`
	RestartDaemon  *bool  `json:"restart_daemon,omitempty" jsonschema:"whether to attempt a graceful restart of antigravity-cli-daemon if idle (default: true)"`
	ForceRestart   bool   `json:"force_restart,omitempty" jsonschema:"if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

// AdoptConversationResult returns the outcome of conversation adoption and daemon restart.
type AdoptConversationResult struct {
	Success             bool   `json:"success" jsonschema:"true if the requested conversion and/or restart succeeded"`
	AlreadyConverted    bool   `json:"already_converted" jsonschema:"true if the conversation was already bound to the target project with an effective title (idempotent no-op)"`
	ConversationID      string `json:"conversation_id,omitempty" jsonschema:"conversation ID that was processed"`
	ProjectID           string `json:"project_id,omitempty" jsonschema:"resolved project UUID"`
	ProjectName         string `json:"project_name,omitempty" jsonschema:"human-readable name of the target project"`
	Title               string `json:"title,omitempty" jsonschema:"title assigned or retained on the conversation"`
	DaemonRestarted     bool   `json:"daemon_restarted" jsonschema:"true if antigravity-cli-daemon was successfully restarted"`
	DaemonRestartReason string `json:"daemon_restart_reason,omitempty" jsonschema:"explanation of restart outcome"`
	ActiveSessionsCount int    `json:"active_sessions_count,omitempty" jsonschema:"number of currently active/running sessions detected if restart was skipped"`
}

type projectConversationStats struct {
	Count      int
	LastActive string
}

func parseSQLiteTimestamp(raw string) (time.Time, string) {
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

func openReadonlyDB(dbPath string) (*sql.DB, bool, error) {
	if dbPath == "" {
		return nil, false, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Connect using read-only query mode to avoid any write locks
	dsn := fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, fmt.Errorf("open sqlite db: %w", err)
	}
	return db, true, nil
}

// GetProjectConversationStats inspects conversation_summaries.db and computes
// conversation count and latest modified timestamp for each project_id.
func (m *Manager) GetProjectConversationStats() (map[string]projectConversationStats, error) {
	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}
	db, ok, err := openReadonlyDB(dbPath)
	if err != nil || !ok {
		return map[string]projectConversationStats{}, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT project_id, count(*), max(last_modified_time) FROM conversation_summaries GROUP BY project_id")
	if err != nil {
		return map[string]projectConversationStats{}, nil
	}
	defer rows.Close()

	stats := make(map[string]projectConversationStats)
	for rows.Next() {
		var pid sql.NullString
		var count int
		var maxTime sql.NullString
		if err := rows.Scan(&pid, &count, &maxTime); err != nil {
			continue
		}
		_, formatted := parseSQLiteTimestamp(maxTime.String)
		stats[pid.String] = projectConversationStats{
			Count:      count,
			LastActive: formatted,
		}
	}
	return stats, nil
}

// ListConversations queries conversation sessions with filtering and pagination.
func (m *Manager) ListConversations(in ListConversationsInput) (*ListConversationsResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}
	db, ok, err := openReadonlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &ListConversationsResult{Conversations: []ConversationSummaryItem{}}, nil
	}
	defer db.Close()

	// Check if conversations_fts exists for archived status check
	hasFTS := false
	var tableName string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='conversations_fts'").Scan(&tableName); err == nil && tableName == "conversations_fts" {
		hasFTS = true
	}

	// Build WHERE clause
	var whereClauses []string
	var args []any

	if in.ProjectID != "" {
		whereClauses = append(whereClauses, "c.project_id = ?")
		args = append(args, in.ProjectID)
	}

	if in.Query != "" {
		whereClauses = append(whereClauses, "c.title LIKE ?")
		args = append(args, "%"+in.Query+"%")
	}

	if !in.IncludeArchived && hasFTS {
		whereClauses = append(whereClauses, "COALESCE(f.archived, 0) = 0")
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	var fromClause string
	if hasFTS {
		fromClause = "FROM conversation_summaries c LEFT JOIN (SELECT conversation_id, MAX(archived) as archived FROM conversations_fts GROUP BY conversation_id) f ON c.conversation_id = f.conversation_id"
	} else {
		fromClause = "FROM conversation_summaries c"
	}

	countQuery := fmt.Sprintf("SELECT COUNT(*) %s %s", fromClause, whereSQL)
	var totalCount int
	if err := db.QueryRow(countQuery, args...).Scan(&totalCount); err != nil {
		return nil, fmt.Errorf("count conversations: %w", err)
	}

	// Select fields
	var selectCols string
	if hasFTS {
		selectCols = "c.conversation_id, c.title, c.project_id, c.status, c.step_count, c.workspace_uris, c.last_modified_time, COALESCE(f.archived, 0)"
	} else {
		selectCols = "c.conversation_id, c.title, c.project_id, c.status, c.step_count, c.workspace_uris, c.last_modified_time, 0"
	}

	dataQuery := fmt.Sprintf("SELECT %s %s %s ORDER BY c.last_modified_time DESC LIMIT ? OFFSET ?", selectCols, fromClause, whereSQL)
	queryArgs := append(slicesClone(args), limit, offset)

	rows, err := db.Query(dataQuery, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query conversations: %w", err)
	}
	defer rows.Close()

	// Discover cloud projects to set IsCloudSynced
	cloudProjects := m.discoverCloudProjectIDs()

	var items []ConversationSummaryItem
	for rows.Next() {
		var (
			cid         sql.NullString
			title       sql.NullString
			pid         sql.NullString
			status      sql.NullString
			stepCount   int
			urisRaw     sql.NullString
			lastModRaw  sql.NullString
			archivedNum int
		)
		if err := rows.Scan(&cid, &title, &pid, &status, &stepCount, &urisRaw, &lastModRaw, &archivedNum); err != nil {
			continue
		}

		var uris []string
		if urisRaw.Valid && urisRaw.String != "" {
			_ = json.Unmarshal([]byte(urisRaw.String), &uris)
		}
		if uris == nil {
			uris = []string{}
		}

		_, formattedTime := parseSQLiteTimestamp(lastModRaw.String)

		isSynced := false
		if pid.Valid && cloudProjects[pid.String] {
			isSynced = true
		}

		items = append(items, ConversationSummaryItem{
			ConversationID:   cid.String,
			Title:            title.String,
			ProjectID:        pid.String,
			IsCloudSynced:    isSynced,
			Archived:         archivedNum > 0,
			Status:           status.String,
			StepCount:        stepCount,
			WorkspaceURIs:    uris,
			LastModifiedTime: formattedTime,
		})
	}
	if items == nil {
		items = []ConversationSummaryItem{}
	}

	hasMore := (offset + len(items)) < totalCount

	return &ListConversationsResult{
		Conversations: items,
		TotalCount:    totalCount,
		HasMore:       hasMore,
	}, nil
}

// discoverCloudProjectIDs reads registered project JSON files and returns IDs
// that are considered cloud-synced (not isWorkspaceOnly, and not cli-default/outside-of-project).
func (m *Manager) discoverCloudProjectIDs() map[string]bool {
	projDir := m.projectsDir
	if projDir == "" {
		projDir = defaultProjectsConfigDir()
	}
	cloudMap := make(map[string]bool)
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return cloudMap
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(projDir, entry.Name()))
		if err != nil {
			continue
		}
		var reg projectRegistration
		if err := json.Unmarshal(data, &reg); err != nil || reg.ID == "" {
			continue
		}
		if reg.ID == "default-cli-project" || reg.ID == "outside-of-project" {
			continue
		}
		if reg.IsWorkspaceOnly != nil && *reg.IsWorkspaceOnly {
			continue
		}
		cloudMap[reg.ID] = true
	}
	return cloudMap
}

func slicesClone[T any](s []T) []T {
	if s == nil {
		return nil
	}
	c := make([]T, len(s))
	copy(c, s)
	return c
}

const daemonRestartCooldown = 15 * time.Second

// AdoptConversation binds a conversation to a specified project, assigns or retains a title,
// and optionally triggers a graceful sync daemon restart if no sessions are actively running.
// If in.ConversationID is empty, it operates as a standalone graceful sync daemon restart.
func (m *Manager) AdoptConversation(ctx context.Context, in AdoptConversationInput) (*AdoptConversationResult, error) {
	res := &AdoptConversationResult{}

	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}

	// Case 1: Standalone graceful daemon restart (ConversationID is omitted)
	if strings.TrimSpace(in.ConversationID) == "" {
		restarted, reason, activeCount, err := m.restartSyncDaemon(ctx, in.ForceRestart)
		if err != nil {
			return nil, err
		}
		res.Success = true
		res.DaemonRestarted = restarted
		res.DaemonRestartReason = reason
		res.ActiveSessionsCount = activeCount
		return res, nil
	}

	// Case 2: Adopt Conversation into a Project
	targetProj, err := m.resolveProjectTarget(in.Project)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("conversation database not found at %s: %w", dbPath, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open conversation db for write: %w", err)
	}
	defer db.Close()

	// Query current conversation row
	var (
		curProjectID sql.NullString
		curWorkspace sql.NullString
		curTitle     sql.NullString
		curPreview   sql.NullString
	)
	err = db.QueryRow(
		"SELECT project_id, workspace_uris, title, preview FROM conversation_summaries WHERE conversation_id = ?",
		in.ConversationID,
	).Scan(&curProjectID, &curWorkspace, &curTitle, &curPreview)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("conversation %q not found in conversation_summaries.db", in.ConversationID)
		}
		return nil, fmt.Errorf("query conversation: %w", err)
	}

	// Determine final title
	finalTitle := strings.TrimSpace(in.Title)
	if finalTitle == "" {
		if curTitle.Valid && strings.TrimSpace(curTitle.String) != "" {
			finalTitle = curTitle.String
		} else if curPreview.Valid && strings.TrimSpace(curPreview.String) != "" {
			prev := strings.ReplaceAll(curPreview.String, "\n", " ")
			prev = strings.ReplaceAll(prev, "\r", "")
			prev = strings.TrimSpace(prev)
			runes := []rune(prev)
			if len(runes) > 60 {
				finalTitle = string(runes[:60]) + "..."
			} else {
				finalTitle = string(runes)
			}
		} else {
			shortID := in.ConversationID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			finalTitle = "Adopted Conversation " + shortID
		}
	}

	targetWorkspaceBytes, _ := json.Marshal([]string{"file://" + filepath.ToSlash(targetProj.Root)})
	targetWorkspaceStr := string(targetWorkspaceBytes)

	// Idempotency check: already in target project with desired title and workspace
	isAlreadyConverted := (curProjectID.String == targetProj.ID) &&
		(curTitle.String == finalTitle) &&
		(curWorkspace.String == targetWorkspaceStr || targetProj.Root == "")

	if !isAlreadyConverted {
		_, err = db.Exec(
			"UPDATE conversation_summaries SET project_id = ?, workspace_uris = ?, title = ? WHERE conversation_id = ?",
			targetProj.ID, targetWorkspaceStr, finalTitle, in.ConversationID,
		)
		if err != nil {
			return nil, fmt.Errorf("update conversation summary: %w", err)
		}
	}

	res.Success = true
	res.AlreadyConverted = isAlreadyConverted
	res.ConversationID = in.ConversationID
	res.ProjectID = targetProj.ID
	res.ProjectName = targetProj.Name
	res.Title = finalTitle

	// Handle daemon restart if requested (default: true)
	restart := true
	if in.RestartDaemon != nil {
		restart = *in.RestartDaemon
	}

	if !restart {
		res.DaemonRestarted = false
		res.DaemonRestartReason = "restart disabled by caller"
		return res, nil
	}

	restarted, reason, activeCount, err := m.restartSyncDaemon(ctx, in.ForceRestart)
	if err != nil {
		res.DaemonRestarted = false
		res.DaemonRestartReason = fmt.Sprintf("restart failed: %v", err)
		return res, nil
	}
	res.DaemonRestarted = restarted
	res.DaemonRestartReason = reason
	res.ActiveSessionsCount = activeCount

	return res, nil
}

func (m *Manager) resolveProjectTarget(projectQuery string) (*ProjectInfo, error) {
	query := strings.TrimSpace(projectQuery)
	if query == "" {
		return nil, errors.New("project identifier or name is required")
	}
	projects, err := m.ListProjects("")
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	lowerQuery := strings.ToLower(query)

	// 1. Exact ID match
	for _, p := range projects {
		if strings.EqualFold(p.ID, query) {
			return &p, nil
		}
	}

	// 2. Exact Name match
	for _, p := range projects {
		if strings.EqualFold(p.Name, query) {
			return &p, nil
		}
	}

	// 3. Exact Root directory match
	cleanQuery := filepath.Clean(query)
	for _, p := range projects {
		if filepath.Clean(p.Root) == cleanQuery {
			return &p, nil
		}
	}

	// 4. Substring match
	var matches []ProjectInfo
	for _, p := range projects {
		if strings.Contains(strings.ToLower(p.ID), lowerQuery) ||
			strings.Contains(strings.ToLower(p.Name), lowerQuery) ||
			strings.Contains(strings.ToLower(p.Root), lowerQuery) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		var names []string
		for _, match := range matches {
			names = append(names, fmt.Sprintf("%s (%s)", match.Name, match.ID))
		}
		return nil, fmt.Errorf("project query %q is ambiguous: matches %s", query, strings.Join(names, ", "))
	}

	return nil, fmt.Errorf("project %q not found in registered Antigravity projects or /data", query)
}

func (m *Manager) restartSyncDaemon(ctx context.Context, force bool) (restarted bool, reason string, activeCount int, err error) {
	m.daemonRestartMu.Lock()
	defer m.daemonRestartMu.Unlock()

	// 1. Cooldown check (prevent restart thrashing within 15 seconds)
	now := time.Now()
	if !m.lastDaemonRestart.IsZero() && now.Sub(m.lastDaemonRestart) < daemonRestartCooldown {
		return false, "skipped: cooldown active (recently restarted)", 0, nil
	}

	// 2. Idle check (unless force is true)
	if !force {
		dbPath := m.conversationsDBPath
		if dbPath == "" {
			dbPath = defaultConversationsDBPath()
		}
		if _, statErr := os.Stat(dbPath); statErr == nil {
			db, ok, openErr := openReadonlyDB(dbPath)
			if openErr == nil && ok {
				defer db.Close()
				var running int
				_ = db.QueryRow("SELECT COUNT(*) FROM conversation_summaries WHERE not_fully_idle != 0 OR status = 'CASCADE_RUN_STATUS_RUNNING'").Scan(&running)
				if running > 0 {
					return false, fmt.Sprintf("skipped: %d active session(s) running; changes saved to DB and will sync in next automatic cycle", running), running, nil
				}
			}
		}
	}

	// 3. Perform restart
	cmdFn := m.restartDaemonCmd
	if cmdFn == nil {
		cmdFn = func(c context.Context) error {
			return probeCmd(c, "systemctl", "--user", "restart", "antigravity-cli-daemon.service").Run()
		}
	}

	if err := cmdFn(ctx); err != nil {
		return false, fmt.Sprintf("restart failed: %v", err), 0, err
	}

	m.lastDaemonRestart = time.Now()
	return true, "restarted gracefully", 0, nil
}
