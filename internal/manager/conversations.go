package manager

import (
	"database/sql"
	"encoding/json"
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
