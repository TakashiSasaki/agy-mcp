package manager

import (
	"context"
	"fmt"
	"io"
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
	const maxBytes = 32 << 20
	path := filepath.Join(filepath.Dir(dbPath), "agyhub_summaries_proto.pb")
	file, info, err := jobstore.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("daemon summary snapshot exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("daemon summary snapshot exceeds %d bytes", maxBytes)
	}
	fields, err := protohelper.ParseFields(data)
	if err != nil {
		return nil, err
	}
	conversations := make(map[string]daemonConversation)
	for _, field := range fields {
		if field.FieldNum != 1 || field.WireType != 2 {
			continue
		}
		id, idOK := protohelper.GetFieldBytes(field.LengthBytes, 1)
		summary, summaryOK := protohelper.GetFieldBytes(field.LengthBytes, 2)
		if !idOK || len(id) == 0 || !summaryOK {
			return nil, fmt.Errorf("invalid entry in daemon summary snapshot")
		}
		key := string(id)
		if _, found := conversations[key]; found {
			return nil, fmt.Errorf("duplicate conversation in daemon summary snapshot")
		}
		annotations, _ := protohelper.GetFieldBytes(summary, summaryAnnotationsField)
		title, _ := protohelper.GetFieldBytes(annotations, annotationTitleField)
		metadata, _ := protohelper.GetFieldBytes(summary, summaryTrajectoryField)
		project, _ := protohelper.GetFieldBytes(metadata, trajectoryProjectField)
		conversations[key] = daemonConversation{project: string(project), title: string(title)}
	}
	return conversations, nil
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
