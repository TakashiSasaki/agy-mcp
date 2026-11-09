package mcptools

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyListConversations  = "agy_list_conversations"
	toolAgyGetConversation   = "agy_get_conversation"
	toolAgyAdoptConversation = "agy_adopt_conversation"
	toolAgyRestartSyncDaemon = "agy_restart_sync_daemon"
)

type listConversationsInput struct {
	ConversationID  string `json:"conversation_id,omitempty" jsonschema:"optional exact conversation ID (UUID) to filter"`
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

type getConversationInput struct {
	ConversationID string `json:"conversation_id" jsonschema:"required: true, exact conversation UUID to inspect"`
}

type getConversationOutput struct {
	Conversation *manager.ConversationSummaryItem `json:"conversation,omitempty" jsonschema:"conversation session details"`
}

type adoptConversationInput struct {
	ConversationID string `json:"conversation_id" jsonschema:"required: true, conversation ID (UUID) to adopt into a registered project"`
	Project        string `json:"project,omitempty" jsonschema:"target project ID, name, or directory path to bind the conversation to (defaults to current project if empty)"`
	Title          string `json:"title,omitempty" jsonschema:"optional title to assign; if omitted, retains existing non-empty title or derives from user prompt preview"`
	RestartDaemon  *bool  `json:"restart_daemon,omitempty" jsonschema:"whether to attempt a graceful restart of antigravity-cli-daemon if idle (default: false)"`
	ForceRestart   bool   `json:"force_restart,omitempty" jsonschema:"if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

type adoptConversationOutput struct {
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

type restartSyncDaemonInput struct {
	Force bool `json:"force,omitempty" jsonschema:"optional, if true, bypasses the idle check and forces daemon restart (dangerous, defaults to false)"`
}

type restartSyncDaemonOutput struct {
	Success             bool   `json:"success" jsonschema:"true if restart completed or was cleanly skipped due to active sessions"`
	DaemonRestarted     bool   `json:"daemon_restarted" jsonschema:"true if antigravity-cli-daemon was restarted"`
	DaemonRestartReason string `json:"daemon_restart_reason" jsonschema:"explanation of restart outcome"`
	ActiveSessionsCount int    `json:"active_sessions_count" jsonschema:"number of currently active sessions detected"`
}

var annAdopt = &mcp.ToolAnnotations{
	DestructiveHint: new(true),
	IdempotentHint:  true,
	OpenWorldHint:   new(false),
}

var annRestartDaemon = &mcp.ToolAnnotations{
	DestructiveHint: new(true),
	IdempotentHint:  false,
	OpenWorldHint:   new(false),
}

func registerConversations(s *toolServer, mgr *manager.Manager) {
	// 1. agy_list_conversations
	addTool(s, &mcp.Tool{
		Name:        toolAgyListConversations,
		Title:       "List Antigravity conversations",
		Annotations: annReadLocal,
		Description: "Use when: enumerating, searching, or filtering historical Antigravity conversations across all projects from conversation_summaries.db. Do not use when: inspecting a single known conversation by ID (use agy_get_conversation instead), or when you only need recently active interactive sessions (use list_sessions instead). Returns conversation IDs, titles, associated project IDs, cloud sync status (is_cloud_synced), archive status, execution status, step counts, workspace URIs, and last modified timestamps. Use agy_list_projects to discover registered project IDs or agy_adopt_conversation to adopt orphan conversations into a project.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listConversationsInput) (*mcp.CallToolResult, listConversationsOutput, error) {
		res, err := mgr.ListConversations(manager.ListConversationsInput{
			ConversationID:  in.ConversationID,
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

	// 2. agy_get_conversation
	addTool(s, &mcp.Tool{
		Name:        toolAgyGetConversation,
		Title:       "Get Antigravity conversation by ID",
		Annotations: annReadLocal,
		Description: "Use when: inspecting or verifying the exact metadata, project binding, and cloud sync status of a specific conversation by ID. Do not use when: searching or listing multiple conversations across projects (use agy_list_conversations instead), or when inspecting cached active CLI sessions (use list_sessions instead). Use agy_adopt_conversation to bind the conversation to a cloud-synced project if needed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getConversationInput) (*mcp.CallToolResult, getConversationOutput, error) {
		conv, err := mgr.GetConversation(strings.TrimSpace(in.ConversationID))
		if err != nil {
			return nil, getConversationOutput{}, err
		}
		return nil, getConversationOutput{
			Conversation: conv,
		}, nil
	})

	// 3. agy_adopt_conversation
	addTool(s, &mcp.Tool{
		Name:        toolAgyAdoptConversation,
		Title:       "Adopt conversation into cloud project",
		Annotations: annAdopt,
		Description: "Use when: adopting or converting a local Antigravity conversation (e.g. started with -p or without a title) into a registered cloud-synced project by binding project metadata, deriving/updating its title, and ensuring atomic persistence across DB, protobuf caches, and Connect RPC. Do not use when: you only want to restart the sync daemon without modifying a conversation (use agy_restart_sync_daemon instead). Use agy_get_conversation to verify post-adoption status, agy_list_conversations to find conversations, or agy_list_projects to discover target projects.",
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
			Success:               res.Success,
			AlreadyConverted:      res.AlreadyConverted,
			ConversationID:        res.ConversationID,
			ProjectID:             res.ProjectID,
			ProjectName:           res.ProjectName,
			Title:                 res.Title,
			MetadataUpdated:       res.MetadataUpdated,
			TrajectoryBlobUpdated: res.TrajectoryBlobUpdated,
			AnnotationsUpdated:     res.AnnotationsUpdated,
			RPCNotified:           res.RPCNotified,
			DaemonRestarted:       res.DaemonRestarted,
			DaemonRestartReason:   res.DaemonRestartReason,
			ActiveSessionsCount:   res.ActiveSessionsCount,
			PostRestartVerified:   res.PostRestartVerified,
		}, nil
	})

	// 4. agy_restart_sync_daemon
	addTool(s, &mcp.Tool{
		Name:        toolAgyRestartSyncDaemon,
		Title:       "Graceful restart antigravity sync daemon",
		Annotations: annRestartDaemon,
		Description: "Use when: explicitly requesting a graceful restart of the Antigravity Language Server / remote-control sync daemon with active session protection (idle guard) and a 15-second cooldown guard. Do not use when: you want to adopt a conversation into a project (use agy_adopt_conversation instead). Use agy_get_conversation or agy_list_conversations to inspect conversation states.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in restartSyncDaemonInput) (*mcp.CallToolResult, restartSyncDaemonOutput, error) {
		res, err := mgr.RestartSyncDaemon(ctx, in.Force)
		if err != nil {
			return nil, restartSyncDaemonOutput{}, err
		}
		if res == nil {
			return nil, restartSyncDaemonOutput{Success: false}, nil
		}
		return nil, restartSyncDaemonOutput{
			Success:             res.Success,
			DaemonRestarted:     res.DaemonRestarted,
			DaemonRestartReason: res.DaemonRestartReason,
			ActiveSessionsCount: res.ActiveSessionsCount,
		}, nil
	})
}
