package manager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
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
	IsCloudSynced    bool     `json:"is_cloud_synced" jsonschema:"true if the conversation belongs to a registered cloud-synced project"`
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
	Project        string `json:"project,omitempty" jsonschema:"target project ID, name, or directory path to bind the conversation to (defaults to current project if empty)"`
	Title          string `json:"title,omitempty" jsonschema:"optional title to assign; if omitted, retains existing non-empty title or derives from user prompt preview"`
	RestartDaemon  *bool  `json:"restart_daemon,omitempty" jsonschema:"whether to attempt a graceful restart of antigravity-cli-daemon if idle (default: false)"`
	ForceRestart   bool   `json:"force_restart,omitempty" jsonschema:"if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

// AdoptConversationResult returns the outcome of conversation adoption and verification.
type AdoptConversationResult struct {
	Success               bool   `json:"success" jsonschema:"true if the adoption succeeded"`
	AlreadyConverted      bool   `json:"already_converted" jsonschema:"true if the conversation was already bound to the target project with the desired title and workspace"`
	ConversationID        string `json:"conversation_id" jsonschema:"conversation ID that was processed"`
	ProjectID             string `json:"project_id" jsonschema:"resolved project UUID"`
	ProjectName           string `json:"project_name" jsonschema:"human-readable name of the target project"`
	Title                 string `json:"title" jsonschema:"title assigned or retained on the conversation"`
	MetadataUpdated       bool   `json:"metadata_updated" jsonschema:"true if conversation_summaries.db columns and raw_summary blob were updated"`
	TrajectoryBlobUpdated bool   `json:"trajectory_blob_updated" jsonschema:"true if conversations/<id>.db trajectory_metadata_blob was updated"`
	AnnotationsUpdated     bool   `json:"annotations_updated" jsonschema:"true if annotations/<id>.pbtxt was updated"`
	RPCNotified           bool   `json:"rpc_notified" jsonschema:"true if Language Server Connect RPC UpdateConversationAnnotations was notified"`
	DaemonRestarted       bool   `json:"daemon_restarted" jsonschema:"true if antigravity-cli-daemon was restarted"`
	DaemonRestartReason   string `json:"daemon_restart_reason,omitempty" jsonschema:"explanation of restart outcome"`
	ActiveSessionsCount   int    `json:"active_sessions_count,omitempty" jsonschema:"number of currently active/running sessions detected if restart was skipped"`
	PostRestartVerified   bool   `json:"post_restart_verified" jsonschema:"true if verified that the updated state persists in conversation_summaries.db"`
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

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open conversation db read-only: %w", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT project_id, count(*), max(last_modified_time) FROM conversation_summaries GROUP BY project_id")
	if err != nil {
		return nil, fmt.Errorf("query project stats: %w", err)
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
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	projects, err := m.ListProjects("")
	if err != nil {
		projects = []ProjectInfo{}
	}
	projectMap := make(map[string]ProjectInfo)
	for _, p := range projects {
		projectMap[p.ID] = p
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open conversation db read-only: %w", err)
	}
	defer db.Close()

	var hasFTS bool
	var ftsCheck int
	_ = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'conversations_fts'").Scan(&ftsCheck)
	hasFTS = ftsCheck > 0

	var whereClauses []string
	var args []interface{}

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
	queryArgs := append(args, limit, offset)

	rows, err := db.Query(dataQuery, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query conversations: %w", err)
	}
	defer rows.Close()

	var items []ConversationSummaryItem
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

		_, lastModRFC := parseSQLiteTimestamp(lastModRaw.String)

		isCloudSynced := false
		syncScope := "unregistered"
		projName := ""
		if proj, ok := projectMap[projectID.String]; ok {
			projName = proj.Name
			syncScope = proj.SyncScope
			if proj.IsCloudSynced && strings.TrimSpace(title.String) != "" {
				isCloudSynced = true
			}
		} else if projectID.String == "default-cli-project" {
			syncScope = "cli_default"
		}

		items = append(items, ConversationSummaryItem{
			ConversationID:   cid,
			Title:            title.String,
			ProjectID:        projectID.String,
			ProjectName:      projName,
			IsCloudSynced:    isCloudSynced,
			SyncScope:        syncScope,
			Archived:         archivedFlag > 0,
			Status:           status.String,
			StepCount:        stepCount,
			WorkspaceURIs:    wsList,
			LastModifiedTime: lastModRFC,
		})
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

// mergeWorkspaceURIs non-destructively preserves existing URIs and appends targetProjRoot if not already present.
func mergeWorkspaceURIs(curJSON string, targetProjRoot string) string {
	var uris []string
	if curJSON != "" {
		_ = json.Unmarshal([]byte(curJSON), &uris)
	}
	if targetProjRoot != "" {
		targetURI := "file://" + filepath.ToSlash(targetProjRoot)
		found := false
		for _, u := range uris {
			if strings.TrimRight(u, "/") == strings.TrimRight(targetURI, "/") {
				found = true
				break
			}
		}
		if !found {
			uris = append(uris, targetURI)
		}
	}
	b, _ := json.Marshal(uris)
	return string(b)
}

// updateTrajectoryMetadataBlob updates Field 18 (project_id) in conversations/<id>.db's trajectory_metadata_blob.
func (m *Manager) updateTrajectoryMetadataBlob(conversationID string, projectID string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	convDBPath := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", conversationID+".db")
	if _, err := os.Stat(convDBPath); err != nil {
		return false
	}

	db, err := sql.Open("sqlite", convDBPath)
	if err != nil {
		return false
	}
	defer db.Close()

	var origData []byte
	err = db.QueryRow("SELECT data FROM trajectory_metadata_blob WHERE id = 'main'").Scan(&origData)
	if err != nil {
		return false
	}

	newData, err := protohelper.SetFieldBytes(origData, 18, []byte(projectID))
	if err != nil {
		return false
	}

	_, err = db.Exec("UPDATE trajectory_metadata_blob SET data = ? WHERE id = 'main'", newData)
	return err == nil
}

// updateAnnotationFile updates or adds title:"..." in annotations/<id>.pbtxt.
func (m *Manager) updateAnnotationFile(conversationID string, title string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	annotDir := filepath.Join(home, ".gemini", "antigravity-cli", "annotations")
	_ = os.MkdirAll(annotDir, 0755)
	annotPath := filepath.Join(annotDir, conversationID+".pbtxt")

	escapedTitle := strings.ReplaceAll(title, `\`, `\\`)
	escapedTitle = strings.ReplaceAll(escapedTitle, `"`, `\"`)
	newTitleEntry := fmt.Sprintf(`title:"%s"`, escapedTitle)

	data, err := os.ReadFile(annotPath)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(annotPath, []byte(newTitleEntry+"\n"), 0644) == nil
		}
		return false
	}

	content := string(data)
	re := regexp.MustCompile(`title:\s*"([^"\\]|\\.)*"`)
	if re.MatchString(content) {
		content = re.ReplaceAllString(content, newTitleEntry)
	} else {
		content = newTitleEntry + " " + strings.TrimSpace(content) + "\n"
	}
	return os.WriteFile(annotPath, []byte(content), 0644) == nil
}

// notifyLanguageServerAnnotationUpdate calls Connect RPC UpdateConversationAnnotations if Language Server is running.
func (m *Manager) notifyLanguageServerAnnotationUpdate(ctx context.Context, conversationID string, title string) bool {
	lsAddr := os.Getenv("ANTIGRAVITY_LS_ADDRESS")
	if lsAddr == "" {
		return false
	}
	csrfToken := os.Getenv("ANTIGRAVITY_CSRF_TOKEN")

	url := fmt.Sprintf("http://%s/exa.language_server_pb.LanguageServerService/UpdateConversationAnnotations", lsAddr)
	payload := map[string]interface{}{
		"cascade_id": conversationID,
		"annotations": map[string]string{
			"title": title,
		},
		"merge_annotations": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if csrfToken != "" {
		req.Header.Set("x-codeium-csrf-token", csrfToken)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// AdoptConversation binds a conversation to a registered project, assigns an effective title,
// and ensures consistent atomic persistence across DB, protobuf blobs, annotation files, and Connect RPC.
func (m *Manager) AdoptConversation(ctx context.Context, in AdoptConversationInput) (*AdoptConversationResult, error) {
	if strings.TrimSpace(in.ConversationID) == "" {
		return nil, errors.New("conversation_id is required")
	}

	res := &AdoptConversationResult{
		ConversationID: in.ConversationID,
	}

	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}

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

	// Check if raw_summary column exists
	var rawCheck int
	_ = db.QueryRow("SELECT count(*) FROM pragma_table_info('conversation_summaries') WHERE name = 'raw_summary'").Scan(&rawCheck)
	hasRawSummary := rawCheck > 0

	// Query current conversation row
	var (
		curProjectID  sql.NullString
		curWorkspace  sql.NullString
		curTitle      sql.NullString
		curPreview    sql.NullString
		curRawSummary []byte
	)
	var queryErr error
	if hasRawSummary {
		queryErr = db.QueryRow(
			"SELECT project_id, workspace_uris, title, preview, raw_summary FROM conversation_summaries WHERE conversation_id = ?",
			in.ConversationID,
		).Scan(&curProjectID, &curWorkspace, &curTitle, &curPreview, &curRawSummary)
	} else {
		queryErr = db.QueryRow(
			"SELECT project_id, workspace_uris, title, preview FROM conversation_summaries WHERE conversation_id = ?",
			in.ConversationID,
		).Scan(&curProjectID, &curWorkspace, &curTitle, &curPreview)
	}
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("conversation %q not found in conversation_summaries.db", in.ConversationID)
		}
		return nil, fmt.Errorf("query conversation: %w", queryErr)
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

	mergedWorkspace := mergeWorkspaceURIs(curWorkspace.String, targetProj.Root)

	// Idempotency check: already in target project with desired title and merged workspace
	isAlreadyConverted := (curProjectID.String == targetProj.ID) &&
		(curTitle.String == finalTitle) &&
		(curWorkspace.String == mergedWorkspace || targetProj.Root == "")

	// 1. Update conversations/<id>.db trajectory_metadata_blob
	trajUpdated := m.updateTrajectoryMetadataBlob(in.ConversationID, targetProj.ID)
	res.TrajectoryBlobUpdated = trajUpdated

	// 2. Update annotations/<id>.pbtxt
	annotUpdated := m.updateAnnotationFile(in.ConversationID, finalTitle)
	res.AnnotationsUpdated = annotUpdated

	// 3. Update conversation_summaries.db columns AND raw_summary protobuf blob
	metadataUpdated := false
	if !isAlreadyConverted {
		var newRawSummary []byte
		if hasRawSummary && len(curRawSummary) > 0 {
			// Update Subfield 1 in Field 15 (title)
			s1, err := protohelper.SetSubFieldBytes(curRawSummary, 15, 1, []byte(finalTitle))
			if err == nil {
				// Update Subfield 18 in Field 17 (project_id)
				s2, err := protohelper.SetSubFieldBytes(s1, 17, 18, []byte(targetProj.ID))
				if err == nil {
					newRawSummary = s2
				}
			}
		}

		if hasRawSummary && len(newRawSummary) > 0 {
			_, err = db.Exec(
				"UPDATE conversation_summaries SET project_id = ?, workspace_uris = ?, title = ?, raw_summary = ? WHERE conversation_id = ?",
				targetProj.ID, mergedWorkspace, finalTitle, newRawSummary, in.ConversationID,
			)
		} else {
			_, err = db.Exec(
				"UPDATE conversation_summaries SET project_id = ?, workspace_uris = ?, title = ? WHERE conversation_id = ?",
				targetProj.ID, mergedWorkspace, finalTitle, in.ConversationID,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("update conversation summary: %w", err)
		}
		metadataUpdated = true
	}
	res.MetadataUpdated = metadataUpdated

	// 4. Notify Language Server Connect RPC if alive
	rpcNotified := m.notifyLanguageServerAnnotationUpdate(ctx, in.ConversationID, finalTitle)
	res.RPCNotified = rpcNotified

	res.Success = true
	res.AlreadyConverted = isAlreadyConverted
	res.ProjectID = targetProj.ID
	res.ProjectName = targetProj.Name
	res.Title = finalTitle

	// 5. Verify post-update state in DB
	var verifyPID, verifyTitle string
	_ = db.QueryRow("SELECT project_id, title FROM conversation_summaries WHERE conversation_id = ?", in.ConversationID).Scan(&verifyPID, &verifyTitle)
	if verifyPID == targetProj.ID && verifyTitle == finalTitle {
		res.PostRestartVerified = true
	}

	// 6. Optional daemon restart (default: false now that Connect RPC live-notifies)
	restart := false
	if in.RestartDaemon != nil {
		restart = *in.RestartDaemon
	}

	if !restart {
		res.DaemonRestarted = false
		res.DaemonRestartReason = "restart not requested (connect rpc and file persistence applied)"
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

// RestartSyncDaemon exposes standalone graceful restart functionality.
func (m *Manager) RestartSyncDaemon(ctx context.Context, force bool) (*DaemonRestartResult, error) {
	restarted, reason, activeCount, err := m.restartSyncDaemon(ctx, force)
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

func (m *Manager) resolveProjectTarget(projectQuery string) (*ProjectInfo, error) {
	projects, err := m.ListProjects("")
	if err != nil {
		return nil, fmt.Errorf("list projects for adoption resolution: %w", err)
	}

	cleanQuery := strings.TrimSpace(projectQuery)
	if cleanQuery == "" {
		for _, p := range projects {
			if p.IsCloudSynced {
				return &p, nil
			}
		}
		if len(projects) > 0 {
			return &projects[0], nil
		}
		return nil, errors.New("no projects available to adopt conversation")
	}

	for _, p := range projects {
		if strings.EqualFold(p.ID, cleanQuery) {
			return &p, nil
		}
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, cleanQuery) {
			return &p, nil
		}
	}
	queryPath := filepath.Clean(cleanQuery)
	for _, p := range projects {
		if p.Root != "" && filepath.Clean(p.Root) == queryPath {
			return &p, nil
		}
	}

	return nil, fmt.Errorf("target project %q could not be resolved by ID, name, or folder path", cleanQuery)
}

// restartSyncDaemon performs an idle-checked graceful restart of antigravity-cli-daemon.
func (m *Manager) restartSyncDaemon(ctx context.Context, force bool) (bool, string, int, error) {
	m.daemonRestartMu.Lock()
	defer m.daemonRestartMu.Unlock()

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
		if _, err := os.Stat(dbPath); err == nil {
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
			if err == nil {
				defer db.Close()
				var running int
				_ = db.QueryRow("SELECT COUNT(*) FROM conversation_summaries WHERE not_fully_idle != 0 OR status = 'CASCADE_RUN_STATUS_RUNNING'").Scan(&running)
				if running > 0 {
					return false, fmt.Sprintf("%d active conversation(s) running; skipped restart to protect active tasks", running), running, nil
				}
			}
		}
	}

	if m.restartDaemonCmd == nil {
		m.restartDaemonCmd = func(c context.Context) error {
			return probeCmd(c, "systemctl", "--user", "restart", "antigravity-cli-daemon.service").Run()
		}
	}

	if err := m.restartDaemonCmd(ctx); err != nil {
		return false, fmt.Sprintf("daemon restart failed: %v", err), 0, err
	}

	m.lastDaemonRestart = time.Now()
	return true, "antigravity-cli-daemon restarted gracefully", 0, nil
}
