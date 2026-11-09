package manager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
)

var adoptionConversationID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AdoptConversation persists the project and title for the same conversation
// ID, then separately observes the Remote Control daemon's summary.
func (m *Manager) AdoptConversation(ctx context.Context, in AdoptConversationInput) (*AdoptConversationResult, error) {
	in.ConversationID = strings.TrimSpace(in.ConversationID)
	if in.ConversationID == "" {
		return nil, errors.New("conversation_id is required")
	}
	if !adoptionConversationID.MatchString(in.ConversationID) {
		return nil, errors.New("conversation_id must be a valid conversation identifier")
	}
	if m.xlock != nil {
		key := keyFor(StartRequest{ConversationID: in.ConversationID})
		locked, err := m.xlock.tryLock(key)
		if err != nil {
			return nil, fmt.Errorf("lock conversation for adoption: %w", err)
		}
		if !locked {
			return nil, errors.New("conversation is busy. Wait for the active operation before adopting")
		}
		defer m.xlock.unlock(key)
	}
	if m.store != nil {
		live, err := m.conversationLive(in.ConversationID)
		if err != nil {
			return nil, err
		}
		if live {
			return nil, errors.New("conversation is busy. Wait for the active job before adopting")
		}
	}

	dbPath := m.conversationsDBPath
	if dbPath == "" {
		dbPath = defaultConversationsDBPath()
	}
	res, err := m.persistAdoption(ctx, dbPath, in)
	if err != nil {
		return nil, err
	}
	m.refreshAdoption(ctx, dbPath, in, res)
	return res, nil
}

type adoptionRecord struct {
	project string
	title   string
	preview string
	raw     []byte
	active  bool
}

func (m *Manager) persistAdoption(ctx context.Context, dbPath string, in AdoptConversationInput) (*AdoptConversationResult, error) {
	if err := checkAdoptionDB(dbPath); err != nil {
		return nil, fmt.Errorf("open conversation summary: %w", err)
	}
	db, err := sql.Open("sqlite", conversationDBURI(dbPath, "rw")+"&_txlock=immediate&_pragma=busy_timeout%282000%29")
	if err != nil {
		return nil, fmt.Errorf("open conversation summary: %w", err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin conversation adoption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := readAdoptionRecord(ctx, tx, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if record.active {
		return nil, errors.New("conversation is busy. Wait for it to become idle before adopting")
	}
	project, err := m.resolveProjectTarget(in.Project, record.project)
	if err != nil {
		return nil, err
	}
	title := adoptionTitle(in.Title, record.title, record.preview, in.ConversationID)
	trajectoryPath := filepath.Join(filepath.Dir(dbPath), "conversations", in.ConversationID+".db")
	trajectoryUpdated, err := updateAdoptionTrajectory(ctx, tx, trajectoryPath, project.ID)
	if err != nil {
		return nil, fmt.Errorf("update conversation trajectory: %w", err)
	}
	summaryUpdated, err := updateAdoptionSummary(ctx, tx, record, in.ConversationID, project.ID, title)
	if err != nil {
		return nil, fmt.Errorf("update conversation summary: %w", err)
	}
	annotationPath := filepath.Join(filepath.Dir(dbPath), "annotations", in.ConversationID+".pbtxt")
	restoreAnnotation, err := writeAdoptionAnnotation(annotationPath, title)
	if err != nil {
		return nil, fmt.Errorf("update conversation annotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if restoreAnnotation != nil {
			err = errors.Join(err, restoreAnnotation())
		}
		return nil, fmt.Errorf("commit conversation adoption: %w", err)
	}
	annotationUpdated := restoreAnnotation != nil
	return &AdoptConversationResult{
		Success:               true,
		AlreadyConverted:      !summaryUpdated && !trajectoryUpdated && !annotationUpdated,
		ConversationID:        in.ConversationID,
		ProjectID:             project.ID,
		ProjectName:           project.Name,
		Title:                 title,
		MetadataUpdated:       summaryUpdated,
		TrajectoryBlobUpdated: trajectoryUpdated,
		AnnotationsUpdated:    annotationUpdated,
	}, nil
}

func checkAdoptionDB(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database path %q must be a regular file", path)
	}
	return nil
}

func conversationDBURI(path, mode string) string {
	u := url.URL{Path: filepath.ToSlash(path)}
	return "file:" + u.EscapedPath() + "?mode=" + mode
}

func readAdoptionRecord(ctx context.Context, tx *sql.Tx, id string) (adoptionRecord, error) {
	var record adoptionRecord
	var hasRawSummary bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'raw_summary')").Scan(&hasRawSummary)
	if err != nil {
		return record, fmt.Errorf("inspect conversation summary schema: %w", err)
	}
	rawColumn := "NULL"
	if hasRawSummary {
		rawColumn = "raw_summary"
	}
	query := `SELECT COALESCE(project_id, ''), COALESCE(title, ''), COALESCE(preview, ''),
		COALESCE(not_fully_idle != 0 OR status IN ('CASCADE_RUN_STATUS_RUNNING', 'RUNNING'), 0), ` + rawColumn +
		" FROM conversation_summaries WHERE conversation_id = ?"
	err = tx.QueryRowContext(ctx, query, id).Scan(&record.project, &record.title, &record.preview, &record.active, &record.raw)
	if errors.Is(err, sql.ErrNoRows) {
		return record, fmt.Errorf("conversation %q not found", id)
	}
	if err != nil {
		return record, fmt.Errorf("read conversation summary: %w", err)
	}
	return record, nil
}

func updateAdoptionTrajectory(ctx context.Context, tx *sql.Tx, path, project string) (bool, error) {
	if err := checkAdoptionDB(path); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "ATTACH DATABASE ? AS adopted_trajectory", conversationDBURI(path, "rw")); err != nil {
		return false, err
	}
	var original []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM adopted_trajectory.trajectory_metadata_blob WHERE id = 'main'").Scan(&original)
	if err != nil {
		return false, err
	}
	updated, err := protohelper.SetFieldBytes(original, trajectoryProjectField, []byte(project))
	if err != nil || bytes.Equal(updated, original) {
		return false, err
	}
	return oneAdoptionRow(tx.ExecContext(ctx, "UPDATE adopted_trajectory.trajectory_metadata_blob SET data = ? WHERE id = 'main'", updated))
}

func updateAdoptionSummary(ctx context.Context, tx *sql.Tx, record adoptionRecord, id, project, title string) (bool, error) {
	if len(record.raw) != 0 {
		updated, err := protohelper.SetSubFieldBytes(record.raw, summaryAnnotationsField, annotationTitleField, []byte(title))
		if err != nil {
			return false, err
		}
		updated, err = protohelper.SetSubFieldBytes(updated, summaryTrajectoryField, trajectoryProjectField, []byte(project))
		if err != nil {
			return false, err
		}
		if !bytes.Equal(updated, record.raw) {
			return oneAdoptionRow(tx.ExecContext(ctx, "UPDATE conversation_summaries SET project_id = ?, title = ?, raw_summary = ? WHERE conversation_id = ?",
				project, title, updated, id))
		}
	}
	if record.project == project && record.title == title {
		return false, nil
	}
	// AGY derives workspace URIs from the original trajectory. Its startup
	// reconciliation restored them after a summary-only append on agy 1.5.3.
	return oneAdoptionRow(tx.ExecContext(ctx, "UPDATE conversation_summaries SET project_id = ?, title = ? WHERE conversation_id = ?", project, title, id))
}

func oneAdoptionRow(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count != 1 {
		return false, fmt.Errorf("expected to update one conversation row, updated %d", count)
	}
	return true, nil
}

// The annotations observed on agy 1.5.3 contain scalar fields and quoted
// strings. Consume unrelated strings and comments as tokens so a title-like
// string in a tag or comment stays intact.
var annotationTokens = regexp.MustCompile(`(?:^|[[:space:],;])title\s*:\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')|"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|#[^\r\n]*`)

func writeAdoptionAnnotation(path, title string) (restore func() error, err error) {
	original, err := readAdoptionAnnotation(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	updated, err := annotationWithTitle(string(original), title)
	if err != nil || updated == string(original) {
		return nil, err
	}
	dir, name := filepath.Dir(path), filepath.Base(path)
	if err := jobstore.MkdirAllDurable(dir, 0o700); err != nil {
		return nil, err
	}
	if err := jobstore.WritePrivateFileAtomic(dir, name, []byte(updated)); err != nil {
		return nil, err
	}
	return func() error {
		current, err := readAdoptionAnnotation(path)
		if err != nil {
			return fmt.Errorf("read annotation for rollback: %w", err)
		}
		if string(current) != updated {
			return errors.New("annotation changed before rollback. Check its title before retrying adoption")
		}
		if !existed {
			return os.Remove(path)
		}
		return jobstore.WritePrivateFileAtomic(dir, name, original)
	}, nil
}

func readAdoptionAnnotation(path string) ([]byte, error) {
	const maxAnnotationBytes = 1 << 20
	file, _, err := jobstore.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxAnnotationBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAnnotationBytes {
		return nil, errors.New("annotation file exceeds size limit")
	}
	return data, nil
}

func annotationWithTitle(content, title string) (string, error) {
	var found []int
	for _, match := range annotationTokens.FindAllStringSubmatchIndex(content, -1) {
		if match[2] < 0 {
			continue
		}
		if found != nil {
			return "", errors.New("annotation contains multiple titles")
		}
		found = match
	}
	quoted := strconv.Quote(title)
	if found == nil {
		return "title:" + quoted + "\n" + content, nil
	}
	if current, err := strconv.Unquote(content[found[2]:found[3]]); err == nil && current == title {
		return content, nil
	}
	return content[:found[2]] + quoted + content[found[3]:], nil
}

func adoptionTitle(requested, current, preview, id string) string {
	if title := strings.TrimSpace(requested); title != "" {
		return title
	}
	if strings.TrimSpace(current) != "" {
		return current
	}
	preview = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", "").Replace(preview))
	if preview == "" {
		return "Adopted Conversation " + id[:min(len(id), 8)]
	}
	runes := []rune(preview)
	if len(runes) > 60 {
		return string(runes[:60]) + "..."
	}
	return preview
}

func (m *Manager) resolveProjectTarget(query, current string) (*ProjectInfo, error) {
	dir := m.projectsDir
	if dir == "" {
		dir = defaultProjectsConfigDir()
	}
	projects, _ := discoverRegisteredProjects(dir)
	query = strings.TrimSpace(query)
	if query == "" {
		for _, project := range projects {
			if project.IsCloudSynced && project.ID == current {
				return project, nil
			}
		}
		return nil, errors.New("project is required for a conversation outside a registered cloud project")
	}
	var best *ProjectInfo
	bestRank := 0
	ambiguous := false
	for _, project := range projects {
		if !project.IsCloudSynced {
			continue
		}
		var rank int
		switch {
		case strings.EqualFold(project.ID, query):
			rank = 3
		case strings.EqualFold(project.Name, query):
			rank = 2
		case filepath.Clean(query) == project.Root:
			rank = 1
		default:
			continue
		}
		switch {
		case rank > bestRank:
			best, bestRank, ambiguous = project, rank, false
		case rank == bestRank && project.ID != best.ID:
			ambiguous = true
		}
	}
	if ambiguous {
		return nil, fmt.Errorf("target project %q is ambiguous. Pass its project ID", query)
	}
	if best == nil {
		return nil, fmt.Errorf("target project %q must be a registered cloud project", query)
	}
	return best, nil
}

func (m *Manager) refreshAdoption(ctx context.Context, dbPath string, in AdoptConversationInput, res *AdoptConversationResult) {
	summaries, _ := readDaemonConversations(dbPath)
	res.CloudSyncState = daemonConversationState(summaries, in.ConversationID, res.ProjectID, res.Title)
	cliCachePath := filepath.Join(filepath.Dir(dbPath), cliSummariesFile)
	target := daemonConversation{project: res.ProjectID, title: res.Title}

	// A title-only RPC is safe once the daemon already has the target project.
	// A daemon holding the previous project needs the complete local record.
	if res.AnnotationsUpdated && summaries[in.ConversationID].project == res.ProjectID {
		res.RPCNotified = notifyLanguageServerAnnotationUpdate(ctx, in.ConversationID, res.Title)
		if res.RPCNotified {
			summaries, _ = readDaemonConversations(dbPath)
			res.CloudSyncState = daemonConversationState(summaries, in.ConversationID, res.ProjectID, res.Title)
		}
	}
	switch {
	case in.RestartDaemon == nil || !*in.RestartDaemon:
		res.DaemonRestartReason = "restart not requested"
		return
	case res.CloudSyncState == ConversationSyncConfirmed && !in.ForceRestart && !hasStaleCLISummary(cliCachePath, in.ConversationID, target):
		res.DaemonRestartReason = "daemon already has the adopted project and title"
		return
	}
	prepare := func(ctx context.Context) error {
		// Recheck the local binding after stopping the daemon in case it
		// flushed its older summary during shutdown.
		in.Project, in.Title = res.ProjectID, res.Title
		persisted, err := m.persistAdoption(ctx, dbPath, in)
		if err != nil {
			return err
		}
		res.MetadataUpdated = res.MetadataUpdated || persisted.MetadataUpdated
		res.TrajectoryBlobUpdated = res.TrajectoryBlobUpdated || persisted.TrajectoryBlobUpdated
		res.AnnotationsUpdated = res.AnnotationsUpdated || persisted.AnnotationsUpdated
		res.AlreadyConverted = res.AlreadyConverted && persisted.AlreadyConverted
		res.CLICacheInvalidated, err = invalidateCachedConversation(cliCachePath, in.ConversationID, target)
		if err != nil {
			return fmt.Errorf("refresh CLI summary: %w", err)
		}
		daemonCachePath := filepath.Join(filepath.Dir(dbPath), daemonSummariesFile)
		res.DaemonCacheInvalidated, err = invalidateCachedConversation(daemonCachePath, in.ConversationID, target)
		if err != nil {
			return fmt.Errorf("refresh daemon summary: %w", err)
		}
		return nil
	}
	restarted, reason, activeCount, err := m.restartSyncDaemon(ctx, in.ForceRestart, prepare)
	res.DaemonRestarted, res.DaemonRestartReason, res.ActiveSessionsCount = restarted, reason, activeCount
	if err != nil || !restarted {
		return
	}
	res.CloudSyncState = waitForDaemonConversation(ctx, dbPath, in.ConversationID, res.ProjectID, res.Title)
	res.PostRestartVerified = res.CloudSyncState == ConversationSyncConfirmed
}

func notifyLanguageServerAnnotationUpdate(ctx context.Context, id, title string) bool {
	address := os.Getenv("ANTIGRAVITY_LS_ADDRESS")
	if address == "" {
		return false
	}
	endpoint := "http://" + address + "/exa.language_server_pb.LanguageServerService/UpdateConversationAnnotations"
	payload := map[string]any{"cascade_id": id, "annotations": map[string]string{"title": title}, "merge_annotations": true}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token := os.Getenv("ANTIGRAVITY_CSRF_TOKEN"); token != "" {
		req.Header.Set("x-codeium-csrf-token", token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}
