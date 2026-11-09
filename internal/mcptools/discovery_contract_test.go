package mcptools

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Mutation guards: runInputSchema's enums, static defaults and examples, plus
// addStartTool's argument guide. The assertions use the catalog clients receive.
func TestRunDiscoveryContract(t *testing.T) {
	for _, tool := range listRegisteredTools(t) {
		if tool.Name != toolAgyRun && tool.Name != toolAgyRunSync {
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			data, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			for field, prop := range schema.Properties {
				if len(prop.Examples) == 0 || strings.TrimSpace(prop.Description) == "" {
					t.Errorf("%s has no example or description", field)
				}
				if !mentions(tool.Description, field) {
					t.Errorf("%s is missing from the tool-level argument guide", field)
				}
			}
			assertRunChoicesAndDefaults(t, &schema)
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			for field, prop := range schema.Properties {
				for _, example := range prop.Examples {
					args := map[string]any{"prompt": "task", "project": "example-project"}
					args[field] = example
					if err := resolved.Validate(args); err != nil {
						t.Errorf("%s example violates advertised schema: %v", field, err)
					}
				}
			}
			for _, text := range []string{"server host", "not filesystem confinement", "not enforced read-only", "same conversation", "not per cwd or caller", "JSON-encoded schema string", "conceptual review", "inspected files", "unverified claims", "specification version", "not server-attested inspection", "effective_model null"} {
				if !strings.Contains(tool.Description, text) {
					t.Errorf("tool-level guide lost %q", text)
				}
			}
		})
	}
}

func assertRunChoicesAndDefaults(t *testing.T, schema *jsonschema.Schema) {
	t.Helper()
	if !slices.Contains(schema.Required, "project") || slices.Contains(schema.Required, "title") {
		t.Errorf("require project and allow an omitted title, got required=%v", schema.Required)
	}
	for field, want := range map[string][]any{
		"mode":     {"", "accept-edits", "auto"},
		"effort":   {"", "low", "medium", "high", "xhigh", "max"},
		"priority": {"", "normal", "optional"},
	} {
		if got := schema.Properties[field].Enum; !reflect.DeepEqual(got, want) {
			t.Errorf("%s enum = %v, want %v", field, got, want)
		}
	}
	for field, want := range map[string]string{"priority": `"normal"`, "sandbox": "false", "continue_latest": "false", "project_rules": "true"} {
		if got := string(schema.Properties[field].Default); got != want {
			t.Errorf("%s default = %s, want %s", field, got, want)
		}
	}
	for _, field := range []string{"cwd", "model", "timeout", "mode", "effort", "agent"} {
		if schema.Properties[field].Default != nil {
			t.Errorf("dynamic %s default must not be inserted before replay normalization", field)
		}
	}
}

// Mutation guard: the statusOutput partial JSON tag must not have omitempty.
// Wire keys are checked separately from the advertised required-field contract.
func TestStatusPartialAlwaysExplicit(t *testing.T) {
	for _, state := range []string{manager.StateRunning, manager.StateDone, manager.StateFailed, manager.StateCancelled} {
		for _, partial := range []bool{false, true} {
			status := toStatusOutput(manager.Status{State: state, Partial: partial})
			for _, output := range []any{status, runSyncOutput{JobID: "test", statusOutput: status}} {
				data, err := json.Marshal(output)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if err := json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				if got, present := wire["partial"]; !present || got != partial {
					t.Errorf("%s partial=%t was omitted/changed: %s", state, partial, data)
				}
			}
		}
	}
	for _, tool := range listRegisteredTools(t) {
		if tool.Name != toolAgyStatus && tool.Name != toolAgyWait && tool.Name != toolAgyRunSync {
			continue
		}
		data, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		if len(schema.AnyOf) != 0 {
			schema = *schema.AnyOf[0] // the successful start/status branch
		}
		if !slices.Contains(schema.Required, "partial") {
			t.Errorf("%s does not require partial", tool.Name)
		}
	}
	mgr, id := stageTerminalJob(t)
	expected, err := mgr.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mgr, nil)
	for _, name := range []string{toolAgyStatus, toolAgyWait} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"job_id": id}})
		if err != nil || res.IsError {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
		if got, present := structuredMap(t, res)["partial"]; !present || got != expected.Partial {
			t.Errorf("%s lost explicit partial: %+v", name, res)
		}
	}
}

// Mutation guards: the first instruction paragraph's execution/provenance
// boundary must survive clients that retain only an initial instructions slice.
func TestInstructionsStartWithExecutionBoundary(t *testing.T) {
	first := serverInstructions[:512]
	for _, want := range []string{"server host", "permission checks disabled", "not confinement", "do not establish read-only", "not correctness", "include_instructions", "tool_json"} {
		if !strings.Contains(first, want) {
			t.Errorf("initial instructions lost %q", want)
		}
	}
}
