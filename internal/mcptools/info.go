package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const toolAgyInfo = "agy_info"

type infoInput struct {
	CheckAuth           bool   `json:"check_auth,omitempty" jsonschema:"explicitly probe model-list access without a model turn; default false leaves auth_status not_checked"`
	Tool                string `json:"tool,omitempty" jsonschema:"optional exact tool name, e.g. agy_run. Return that tool's complete native tools/list descriptor as contract.tool_json, including input/output schemas and property descriptions, without executing the tool. Omit for no descriptor"`
	IncludeInstructions bool   `json:"include_instructions,omitempty" jsonschema:"default false; true returns the full native initialize instructions in contract.instructions, bypassing a client's abbreviated server overview. No public MCP URL or additional credentials are needed"`
}

type infoOutput struct {
	APIVersion         string              `json:"api_version" jsonschema:"version of this diagnostic response contract"`
	ServerVersion      string              `json:"server_version" jsonschema:"running binary version including fork revision when available"`
	Runtime            manager.RuntimeInfo `json:"runtime" jsonschema:"effective server defaults, CLI checks, process-local capacity and enforcement capabilities"`
	MaxInlineWait      string              `json:"max_inline_wait" jsonschema:"maximum duration a wait or synchronous call can block this transport"`
	Quota              *quotaSummaryOutput `json:"quota,omitempty" jsonschema:"cached quota reading, if available; this tool never refreshes quota"`
	ToolNames          []string            `json:"tool_names" jsonschema:"sorted tool names actually registered by this server; compare with the caller's visible tools"`
	ToolCount          int                 `json:"tool_count" jsonschema:"number of tools in this server's complete catalog"`
	CatalogSHA256      string              `json:"catalog_sha256" jsonschema:"SHA-256 of the name-sorted catalog encoded as canonical JSON, including input/output schemas; matches request logs"`
	InstructionsBytes  int                 `json:"instructions_bytes" jsonschema:"UTF-8 byte length of the full native server instructions, before client display truncation"`
	InstructionsSHA256 string              `json:"instructions_sha256" jsonschema:"SHA-256 of the UTF-8 bytes of the complete native server instructions"`
	Contract           *infoContractOutput `json:"contract,omitempty" jsonschema:"opt-in native MCP contract evidence; absent unless tool or include_instructions is requested. Same catalog and instructions as native discovery, not evidence that the client displayed them intact"`
}

type infoContractOutput struct {
	ToolJSON     string `json:"tool_json,omitempty" jsonschema:"complete JSON-encoded tools/list descriptor for the requested tool, including property descriptions, examples, enums and schemas; compare parsed JSON with tools/list rather than relying on object key order"`
	ToolSHA256   string `json:"tool_sha256,omitempty" jsonschema:"SHA-256 of the exact UTF-8 bytes in tool_json, not the whole catalog digest"`
	Instructions string `json:"instructions,omitempty" jsonschema:"full untruncated native initialize instructions when requested; byte length and SHA-256 are the top-level instructions_bytes and instructions_sha256"`
}

func textSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// Return only definitions already registered in this process. Tool names never
// select a file, invoke a handler or expose prompts, jobs or credentials.
func inspectInfoContract(catalog []*mcp.Tool, in infoInput) (*infoContractOutput, error) {
	out := &infoContractOutput{}
	if in.IncludeInstructions {
		out.Instructions = serverInstructions
	}
	if in.Tool == "" {
		return out, nil
	}
	for _, tool := range catalog {
		if tool.Name != in.Tool {
			continue
		}
		data, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("encode registered tool %q: %w", in.Tool, err)
		}
		out.ToolJSON = string(data)
		out.ToolSHA256 = textSHA256(out.ToolJSON)
		return out, nil
	}
	return nil, fmt.Errorf("unknown tool %q; use tool_names for exact registered names", in.Tool)
}

func registerInfo(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name: toolAgyInfo, Title: "Inspect agy server capabilities", Annotations: annReadExternal,
		Description: "Inspect this running server's host, defaults, versions and native MCP contract without an agent turn. " +
			"Optional tool (exact name, e.g. agy_run) returns its complete tools/list definition as contract.tool_json; include_instructions true returns full initialize instructions. These use the existing connection and require no public MCP URL. Defaults omit both bodies. instructions_bytes and instructions_sha256 always identify the complete instructions; contract.tool_sha256 hashes the exact returned descriptor bytes. Compare these native values with the caller-visible declarations to locate display truncation; this tool cannot repair client display or refresh metadata. All cwd, dirs and schema-file paths refer to this server host. " +
			"It runs a CLI version check; check_auth explicitly adds a model-list probe, which does not prove that every model can execute a task. " +
			"Compare tool_names with the tools exposed by your connection to detect stale metadata. Missing caller-visible tools require a connection metadata refresh; this tool cannot refresh the client. " +
			"Use agy_usage for a fresh quota reading, agy_list_jobs to recover existing work, and agy_run to start authorized work. read_only_enforced is false; a review prompt or sandbox flag does not establish a verified read-only backend.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in infoInput) (*mcp.CallToolResult, infoOutput, error) {
		var contract *infoContractOutput
		if in.Tool != "" || in.IncludeInstructions {
			var err error
			contract, err = inspectInfoContract(s.catalog, in)
			if err != nil {
				return nil, infoOutput{}, err
			}
		}
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
			InstructionsBytes: len(serverInstructions), InstructionsSHA256: textSHA256(serverInstructions), Contract: contract,
		}, nil
	})
}
