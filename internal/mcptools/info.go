package mcptools

import (
	"context"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const toolAgyInfo = "agy_info"

type infoInput struct {
	CheckAuth bool `json:"check_auth,omitempty" jsonschema:"explicitly probe model-list access without a model turn; default false leaves auth_status not_checked"`
}

type infoOutput struct {
	APIVersion    string              `json:"api_version" jsonschema:"version of this diagnostic response contract"`
	ServerVersion string              `json:"server_version" jsonschema:"running binary version including fork revision when available"`
	Runtime       manager.RuntimeInfo `json:"runtime" jsonschema:"effective server defaults, CLI checks, process-local capacity and enforcement capabilities"`
	MaxInlineWait string              `json:"max_inline_wait" jsonschema:"maximum duration a wait or synchronous call can block this transport"`
	Quota         *quotaSummaryOutput `json:"quota,omitempty" jsonschema:"cached quota reading, if available; this tool never refreshes quota"`
	ToolNames     []string            `json:"tool_names" jsonschema:"sorted tool names actually registered by this server; compare with the caller's visible tools"`
	ToolCount     int                 `json:"tool_count" jsonschema:"number of tools in this server's complete catalog"`
	CatalogSHA256 string              `json:"catalog_sha256" jsonschema:"SHA-256 of the name-sorted catalog encoded as canonical JSON, including input/output schemas; matches request logs"`
}

func registerInfo(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name: toolAgyInfo, Title: "Inspect agy server capabilities", Annotations: annReadExternal,
		Description: "Inspect this running server's host, platform, defaults, catalog fingerprint, cached quota and execution limits without an agent turn. All cwd, dirs and schema-file paths refer to this server host. " +
			"It runs a CLI version check; check_auth explicitly adds a model-list probe, which does not prove that every model can execute a task. " +
			"Compare tool_names with the tools exposed by your connection to detect stale metadata. Missing caller-visible tools require a connection metadata refresh; this tool cannot refresh the client. " +
			"Use agy_usage for a fresh quota reading, agy_list_jobs to recover existing work, and agy_run to start authorized work. read_only_enforced is false; a review prompt or sandbox flag does not establish a verified read-only backend.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in infoInput) (*mcp.CallToolResult, infoOutput, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		runtime, err := mgr.RuntimeInfo(ctx, in.CheckAuth)
		if err != nil {
			return nil, infoOutput{}, err
		}
		names := make([]string, 0, len(s.catalog))
		for _, tool := range s.catalog {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		return nil, infoOutput{
			APIVersion: "1", ServerVersion: ServerVersion(), Runtime: runtime,
			MaxInlineWait: maxSyncWait().String(), Quota: quotaFor(mgr),
			ToolNames: names, ToolCount: len(names), CatalogSHA256: catalogDigest(s.catalog),
		}, nil
	})
}
