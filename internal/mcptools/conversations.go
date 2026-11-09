package mcptools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyListConversations = "agy_list_conversations"
	toolAgyAdoptConversation = "agy_adopt_conversation"
)

type listConversationsInput struct {
	ProjectID       string `json:"project_id,omitempty" jsonschema:"optional project ID or UUID to filter conversations (e.g. 'c5f32b28-...', 'default-cli-project', 'outside-of-project')"`
	Query           string `json:"query,omitempty" jsonschema:"optional search term matching conversation title"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"if true, includes archived sessions (default: false)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum number of sessions to return (default: 50, maximum: 200)"`
	Offset          int    `json:"offset,omitempty" jsonschema:"pagination offset (default: 0)"`
}

type listConversationsOutput struct {
	Conversations []manager.ConversationSummaryItem `json:"conversations" jsonschema:"list of discovered conversation sessions"`
	TotalCount    int                                `json:"total_count" jsonschema:"total count of conversations matching the filter criteria"`
	HasMore       bool                               `json:"has_more" jsonschema:"true if more conversations exist beyond the current limit and offset"`
}

type adoptConversationInput struct {
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"optional conversation ID (UUID) to adopt into a cloud-synced project; if omitted, performs a standalone graceful sync daemon restart"`
	Project        string `json:"project,omitempty" jsonschema:"target project ID, name, or directory path to bind the conversation to (required when conversation_id is provided)"`
	Title          string `json:"title,omitempty" jsonschema:"optional title to assign; if omitted, retains existing non-empty title or derives from user prompt preview"`
	RestartDaemon  *bool  `json:"restart_daemon,omitempty" jsonschema:"whether to attempt a graceful restart of antigravity-cli-daemon if idle (default: true)"`
	ForceRestart   bool   `json:"force_restart,omitempty" jsonschema:"if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

type adoptConversationOutput struct {
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

var annAdopt = &mcp.ToolAnnotations{
	DestructiveHint: new(false),
	IdempotentHint:  true,
	OpenWorldHint:   new(false),
}

func registerConversations(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyListConversations,
		Title:       "List Antigravity conversations",
		Annotations: annReadLocal,
		Description: "List and inspect Antigravity conversation sessions from the local conversation database. Returns conversation IDs, human-readable titles, associated project IDs, cloud synchronization status (is_cloud_synced), archive status (archived), execution status, step counts, workspace URIs, and last modified timestamps. Use agy_list_projects to discover registered project IDs or list_sessions for cached interactive CLI sessions. Supports filtering by project ID, title keyword search, and archive inclusion, with pagination controls.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listConversationsInput) (*mcp.CallToolResult, listConversationsOutput, error) {
		res, err := mgr.ListConversations(manager.ListConversationsInput{
			ProjectID:       in.ProjectID,
			Query:           in.Query,
			IncludeArchived: in.IncludeArchived,
			Limit:           in.Limit,
			Offset:          in.Offset,
		})
		if err != nil {
			return nil, listConversationsOutput{}, err
		}
		if res == nil {
			return nil, listConversationsOutput{Conversations: []manager.ConversationSummaryItem{}}, nil
		}
		return nil, listConversationsOutput{
			Conversations: res.Conversations,
			TotalCount:    res.TotalCount,
			HasMore:       res.HasMore,
		}, nil
	})

	addTool(s, &mcp.Tool{
		Name:        toolAgyAdoptConversation,
		Title:       "Adopt conversation into cloud project",
		Annotations: annAdopt,
		Description: "Adopt a conversation into a cloud-synced project and/or perform a graceful sync daemon restart. Binds a local conversation to the target project, assigns an effective title, and safely triggers antigravity-cli-daemon restart when all sessions are idle (skipping restart if active tasks are running). Omit conversation_id to perform a standalone graceful daemon restart. Use agy_list_conversations to inspect conversation status or agy_list_projects to discover target projects. Idempotent across multiple invocations.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in adoptConversationInput) (*mcp.CallToolResult, adoptConversationOutput, error) {
		res, err := mgr.AdoptConversation(ctx, manager.AdoptConversationInput{
			ConversationID: in.ConversationID,
			Project:        in.Project,
			Title:          in.Title,
			RestartDaemon:  in.RestartDaemon,
			ForceRestart:   in.ForceRestart,
		})
		if err != nil {
			return nil, adoptConversationOutput{}, err
		}
		if res == nil {
			return nil, adoptConversationOutput{Success: false}, nil
		}
		return nil, adoptConversationOutput{
			Success:             res.Success,
			AlreadyConverted:    res.AlreadyConverted,
			ConversationID:      res.ConversationID,
			ProjectID:           res.ProjectID,
			ProjectName:         res.ProjectName,
			Title:               res.Title,
			DaemonRestarted:     res.DaemonRestarted,
			DaemonRestartReason: res.DaemonRestartReason,
			ActiveSessionsCount: res.ActiveSessionsCount,
		}, nil
	})
}
