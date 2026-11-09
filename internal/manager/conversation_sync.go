package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	summaryStepCountField   = 2
	summaryModifiedField    = 3
	annotationTitleField    = 1
	summaryTrajectoryField  = 17
	trajectoryProjectField  = 18
	daemonSummariesFile     = "agyhub_summaries_proto.pb"
	cliSummariesFile        = "jetbox_summaries_proto.pb"
)

type daemonConversation struct {
	project  string
	title    string
	revision conversationRevision
}

type conversationRevision struct {
	modified time.Time
	steps    int
}

func (r conversationRevision) latest(other conversationRevision) conversationRevision {
	if other.modified.After(r.modified) {
		return other
	}
	return r
}

func (c daemonConversation) matches(target daemonConversation) bool {
	if c.project != target.project || c.title != target.title {
		return false
	}
	if target.revision.modified.IsZero() {
		return true
	}
	return c.revision.modified.After(target.revision.modified) ||
		(c.revision.modified.Equal(target.revision.modified) && c.revision.steps == target.revision.steps)
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
	return string(key), daemonConversation{
		project: string(project), title: string(title), revision: decodeConversationRevision(data),
	}, nil
}

func decodeConversationRevision(summary []byte) conversationRevision {
	var revision conversationRevision
	fields, err := protohelper.ParseFields(summary)
	if err != nil {
		return revision
	}
	for _, field := range fields {
		switch {
		case field.FieldNum == summaryStepCountField && field.WireType == 0 && field.VarintVal <= math.MaxInt32:
			revision.steps = int(field.VarintVal)
		case field.FieldNum == summaryModifiedField && field.WireType == 2:
			revision.modified = decodeSummaryTimestamp(field.LengthBytes)
		}
	}
	return revision
}

func decodeSummaryTimestamp(data []byte) time.Time {
	fields, err := protohelper.ParseFields(data)
	if err != nil {
		return time.Time{}
	}
	var seconds, nanos int64
	for _, field := range fields {
		switch {
		case field.FieldNum == 1 && field.WireType == 0:
			seconds = int64(field.VarintVal)
		case field.FieldNum == 2 && field.WireType == 0:
			nanos = int64(field.VarintVal)
		}
	}
	if seconds < -62135596800 || seconds > 253402300799 || nanos < 0 || nanos >= int64(time.Second) {
		return time.Time{}
	}
	return time.Unix(seconds, nanos).UTC()
}

// invalidateCachedConversation runs while the daemon is stopped. MEASURED
// against agy 1.5.3: both the daemon and CLI snapshots can restore an old
// binding at startup. Removing the stale entry lets AGY rebuild it.
// Preserve all other protobuf fields byte for byte, including unknown fields.
func invalidateCachedConversation(path, id string, target daemonConversation) (bool, error) {
	return invalidateSummaryEntries(path, func(key string, summary daemonConversation) (selected, stale bool) {
		return key == id, !summary.matches(target)
	})
}

func invalidateSummaryEntries(path string, classify func(string, daemonConversation) (selected, stale bool)) (bool, error) {
	fields, originalInfo, err := readSummarySnapshot(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seen := make(map[string]bool)
	var updated []byte
	removed := false
	for _, field := range fields {
		if field.FieldNum != 1 {
			updated = append(updated, field.Raw...)
			continue
		}
		key, summary, err := decodeDaemonSummary(field)
		if err != nil {
			return false, err
		}
		selected, stale := classify(key, summary)
		if selected {
			if seen[key] {
				return false, errors.New("duplicate target conversation in summary snapshot")
			}
			seen[key] = true
			if stale {
				removed = true
				continue
			}
		}
		updated = append(updated, field.Raw...)
	}
	if !removed {
		return false, nil
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

func daemonConversationState(conversations map[string]daemonConversation, id string, target daemonConversation) string {
	if conversations == nil {
		return ConversationSyncUnavailable
	}
	if got, ok := conversations[id]; ok && got.matches(target) {
		return ConversationSyncConfirmed
	}
	return ConversationSyncPending
}

func waitForDaemonConversation(ctx context.Context, dbPath, id string, target daemonConversation) string {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		conversations, _ := readDaemonConversations(dbPath)
		state := daemonConversationState(conversations, id, target)
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
