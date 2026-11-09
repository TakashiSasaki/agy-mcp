package mcptools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyListConversations = "agy_list_conversations"
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
}
