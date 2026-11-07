package mcptools

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// Keep review limitations on each start tool: clients may abbreviate server
// instructions and omit individual property descriptions.
const reviewEvidenceGuide = `

Review evidence: specify whether this is a conceptual review of supplied assumptions or a repository review. For repository work, give exact files and a revision/diff, and ask the agent to report inspected files, commands actually run, supporting evidence and unverified claims. cwd and git_head identify execution context, not proof that files were inspected. Separate confirmed defects, conditional risks and proposals; an empty findings list is not proof of correctness. For standards/API claims, request the specification version, primary source and implementation support; independently check proposed syntax and counterexamples before adoption. These are agent-reported findings, not server-attested inspection. A done job or partial false does not verify review coverage or correctness. requested_model and selected_model are request/pre-execution metadata; effective_model null means the backend model is unverified, not that the requested model was confirmed.`

// Some clients render argument types without property descriptions. Keep the
// execution boundary and a compact input reference on each start tool too.
const runArgumentGuide = `

Execution boundary: cwd and all file paths belong to the server host, not the caller's machine. Inspect agy_info for hostname, default_cwd, versions and default_model. Omitted cwd uses that server directory. cwd and dirs select workspaces, not filesystem confinement. sandbox (default false) only forwards agy's terminal-restriction flag; filesystem, network and read-only isolation are not verified. Permission checks are disabled. A no-edit prompt is an instruction, not enforced read-only access.

Arguments: prompt is a required, self-contained task. model and agent default to the server/CLI choices; use list_models and list_agents(cwd) for values. effort accepts low, medium, high, xhigh or max, or omit for the CLI default; not every model supports every effort. mode accepts accept-edits or auto, or omit for the CLI default; plan is rejected and no mode enforces read-only access. priority is normal (default) or optional (refuse/defer skippable work when a trusted quota reading is low or cannot map the model). timeout is a positive Go duration such as 20m or 90s, maximum 24h; default is the server setting (built-in 30m), reported by agy_info. dirs adds read/write workspaces, empty by default; relative entries resolve from cwd. project_rules defaults true; false does not reliably prevent cwd/dirs rules and hooks from loading. json_schema is a JSON-encoded schema string, not an object, or an absolute schema-file path on the server; omit for free text. Example: {"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}. artifact_dirs associates explicit server-side evidence directories, empty by default; it does not add execution workspaces. task_label is an optional recovery label, at most 128 characters.

Continuation and retries: conversation_id resumes a specific thread while cwd still selects the execution directory; retain the original cwd unless intentionally changing it. continue_latest defaults false; true resolves the latest thread for canonical cwd, or starts fresh if none is cached. An unreadable or malformed cache refuses new work with conversation_cache_unavailable rather than starting fresh; an existing job with a stored request key is still replayed without that lookup; legacy argument-only records require the cache for comparison. They are mutually exclusive. Concurrent continuations of the same conversation are refused even across different cwd values; fresh runs can share cwd. idempotency_key is optional, scoped to the shared retained job store across restarts and both run tools, not per cwd or caller. The same key and normalized execution inputs return the existing job; changed inputs are rejected. wait, include_result, priority and task_label do not affect matching. Keys expire only when their job is removed; GC uses age from creation (default 24h) and keeps live jobs; replay keeps the original label and does not repeat work.`

const inlineWaitGuide = `

wait is a positive Go duration such as 30s or 1m; omitted or empty uses the effective inline ceiling (built-in 90s, configurable with AGY_MCP_SYNC_WAIT_CAP; see agy_info.max_inline_wait). Larger values clamp to that ceiling. Expiry leaves the job running under timeout; keep its job_id and use agy_wait or agy_status rather than starting it again.`

// runInputSchema augments inferred descriptions with machine-readable choices
// and examples. Empty enum values preserve the existing "use the default"
// spelling. Only static defaults are materialized: cwd, model and timeout must
// remain unset until the manager restores any persisted idempotent request.
func runInputSchema[In any]() *jsonschema.Schema {
	schema := inferredSchema[In]()
	schema.Properties["effort"].Enum = []any{"", agyEffortLow, agyEffortMedium, agyEffortHigh, agyEffortXHigh, agyEffortMax}
	schema.Properties["mode"].Enum = []any{"", agyModeAcceptEdits, agyModeAuto}
	schema.Properties["priority"].Enum = []any{"", "normal", "optional"}
	schema.Properties["priority"].Default = json.RawMessage(`"normal"`)
	schema.Properties["sandbox"].Default = json.RawMessage(`false`)
	schema.Properties["continue_latest"].Default = json.RawMessage(`false`)
	schema.Properties["project_rules"].Default = json.RawMessage(`true`)
	examples := map[string]any{
		"prompt":          "Review README.md setup instructions. Read the file and report inspected revision, file/line evidence, assumptions and checks not run; distinguish confirmed defects from proposals. Do not edit files or run mutating commands. Write findings directly without delegating to another agent.",
		"cwd":             "/srv/project",
		"model":           "gemini-3.8-flash-medium",
		"effort":          agyEffortMedium,
		"mode":            agyModeAcceptEdits,
		"agent":           "reviewer",
		"sandbox":         true,
		"dirs":            []string{"/srv/specifications"},
		"project_rules":   true,
		"conversation_id": "previous-conversation-id",
		"continue_latest": false,
		"timeout":         "20m",
		"json_schema":     `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`,
		"priority":        "optional",
		"idempotency_key": "review-project-20261105-01",
		"task_label":      "README setup review",
		"artifact_dirs":   []string{"/srv/evidence/review-01"},
	}
	for name, example := range examples {
		schema.Properties[name].Examples = []any{example}
	}
	if wait := schema.Properties["wait"]; wait != nil {
		wait.Examples = []any{"30s", "1m"}
	}
	if include := schema.Properties["include_result"]; include != nil {
		include.Examples = []any{false, true}
	}
	return schema
}
