package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/protohelper"
)

const (
	ConversationSyncConfirmed   = "confirmed"
	ConversationSyncPending     = "pending"
	ConversationSyncUnavailable = "unavailable"
	ConversationSyncLocalOnly   = "local_only"

	summaryAnnotationsField = 15
	annotationTitleField    = 1
	summaryTrajectoryField  = 17
	trajectoryProjectField  = 18
	daemonSummariesFile     = "agyhub_summaries_proto.pb"
	cliSummariesFile        = "jetbox_summaries_proto.pb"
)

type daemonConversation struct {
	project string
	title   string
}

// readDaemonConversations reads the Remote Control daemon's own summary
// snapshot. MEASURED against agy 1.5.3: the daemon populates
// agyhub_summaries_proto.pb during startup reconciliation. It stores the
// conversation ID and CascadeTrajectorySummary as map entries in field 1.
// The CLI's jetbox snapshot and SQLite rows can change before this happens.
func readDaemonConversations(dbPath string) (map[string]daemonConversation, error) {
	return readConversationSummaries(filepath.Join(filepath.Dir(dbPath), daemonSummariesFile))
}

func readConversationSummaries(path string) (map[string]daemonConversation, error) {
	fields, _, err := readSummarySnapshot(path)
	if err != nil {
		return nil, err
	}
	conversations := make(map[string]daemonConversation)
	for _, field := range fields {
		if field.FieldNum != 1 {
			continue
		}
		id, summary, err := decodeDaemonSummary(field)
		if err != nil {
			return nil, err
		}
		if _, found := conversations[id]; found {
			return nil, errors.New("duplicate conversation in daemon summary snapshot")
		}
		conversations[id] = summary
	}
	return conversations, nil
}

func readSummarySnapshot(path string) ([]protohelper.Field, os.FileInfo, error) {
	const maxBytes = 32 << 20
	file, info, err := jobstore.OpenRegularFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("summary snapshot exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxBytes {
		return nil, nil, fmt.Errorf("summary snapshot exceeds %d bytes", maxBytes)
	}
	fields, err := protohelper.ParseFields(data)
	return fields, info, err
}

func decodeDaemonSummary(field protohelper.Field) (id string, summary daemonConversation, err error) {
	key, idOK := protohelper.GetFieldBytes(field.LengthBytes, 1)
	data, summaryOK := protohelper.GetFieldBytes(field.LengthBytes, 2)
	if field.WireType != 2 || !idOK || len(key) == 0 || !summaryOK {
		return "", summary, errors.New("invalid entry in daemon summary snapshot")
	}
	annotations, _ := protohelper.GetFieldBytes(data, summaryAnnotationsField)
	title, _ := protohelper.GetFieldBytes(annotations, annotationTitleField)
	metadata, _ := protohelper.GetFieldBytes(data, summaryTrajectoryField)
	project, _ := protohelper.GetFieldBytes(metadata, trajectoryProjectField)
	return string(key), daemonConversation{project: string(project), title: string(title)}, nil
}

// invalidateCachedConversation runs while the daemon is stopped. MEASURED
// against agy 1.5.3: both the daemon and CLI snapshots can restore an old
// binding at startup. Removing the stale entry lets AGY rebuild it.
// Preserve all other protobuf fields byte for byte, including unknown fields.
func invalidateCachedConversation(path, id string, target daemonConversation) (bool, error) {
	fields, originalInfo, err := readSummarySnapshot(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	remove := -1
	seen := false
	for i, field := range fields {
		if field.FieldNum != 1 {
			continue
		}
		key, summary, err := decodeDaemonSummary(field)
		if err != nil {
			return false, err
		}
		if key != id {
			continue
		}
		if seen {
			return false, errors.New("duplicate target conversation in summary snapshot")
		}
		seen = true
		if summary != target {
			remove = i
		}
	}
	if remove < 0 {
		return false, nil
	}
	var updated []byte
	for i, field := range fields {
		if i != remove {
			updated = append(updated, field.Raw...)
		}
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !os.SameFile(originalInfo, currentInfo) || originalInfo.Size() != currentInfo.Size() ||
		!originalInfo.ModTime().Equal(currentInfo.ModTime()) {
		return false, errors.New("summary snapshot changed during refresh. Retry adoption")
	}
	err = jobstore.WritePrivateFileAtomic(filepath.Dir(path), filepath.Base(path), updated)
	return err == nil, err
}

func hasStaleCLISummary(path, id string, target daemonConversation) bool {
	summaries, err := readConversationSummaries(path)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	got, found := summaries[id]
	return found && got != target
}

func daemonConversationState(conversations map[string]daemonConversation, id, project, title string) string {
	if conversations == nil {
		return ConversationSyncUnavailable
	}
	if got, ok := conversations[id]; ok && got.project == project && got.title == title {
		return ConversationSyncConfirmed
	}
	return ConversationSyncPending
}

func waitForDaemonConversation(ctx context.Context, dbPath, id, project, title string) string {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		conversations, _ := readDaemonConversations(dbPath)
		state := daemonConversationState(conversations, id, project, title)
		if state == ConversationSyncConfirmed {
			return state
		}
		select {
		case <-ctx.Done():
			return state
		case <-ticker.C:
		}
	}
}
