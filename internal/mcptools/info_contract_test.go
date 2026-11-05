package mcptools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func contractTestClient(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return connect(t, manager.New(config.Config{
		StateDir: t.TempDir(), AgyPath: filepath.Join(t.TempDir(), "missing-agy"),
	}), nil)
}

// Mutation guards: opt-in body projection, native descriptor serialization,
// instruction selection and independent byte/hash identities.
func TestInfoContractMatchesNativeDiscovery(t *testing.T) {
	cs := contractTestClient(t)
	catalog, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolAgyRun, toolAgyResolveWorkspace, toolAgyInfo} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyInfo, Arguments: map[string]any{"tool": name, "include_instructions": true}})
		if err != nil || res.IsError {
			t.Fatalf("contract: %+v, %v", res, err)
		}
		out := structuredMap(t, res)
		contract, ok := out["contract"].(map[string]any)
		if !ok {
			t.Fatalf("missing opt-in contract: %+v", out)
		}
		body, ok := contract["tool_json"].(string)
		if !ok || body == "" {
			t.Fatal("native tool JSON missing")
		}
		var actual any
		if err := json.Unmarshal([]byte(body), &actual); err != nil {
			t.Fatal(err)
		}
		var wanted any
		for _, tool := range catalog.Tools {
			if tool.Name == name {
				data, err := json.Marshal(tool)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &wanted); err != nil {
					t.Fatal(err)
				}
			}
		}
		if wanted == nil || !reflect.DeepEqual(actual, wanted) {
			t.Fatalf("%s descriptor differs from native tools/list", name)
		}
		digest := sha256.Sum256([]byte(body))
		if contract["tool_sha256"] != hex.EncodeToString(digest[:]) {
			t.Fatal("descriptor hash does not identify returned bytes")
		}
		instructions := contract["instructions"]
		digest = sha256.Sum256([]byte(serverInstructions))
		if instructions != serverInstructions || out["instructions_bytes"] != float64(len(serverInstructions)) || out["instructions_sha256"] != hex.EncodeToString(digest[:]) {
			t.Fatal("instructions are truncated or misidentified")
		}
	}
	for _, include := range []bool{false, true} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyInfo, Arguments: map[string]any{"include_instructions": include}})
		if err != nil || res.IsError {
			t.Fatalf("instructions only/default: %+v, %v", res, err)
		}
		out := structuredMap(t, res)
		if !include && out["contract"] != nil {
			t.Fatal("default info call returned heavy contract bodies")
		}
		if include {
			contract := out["contract"].(map[string]any)
			if contract["instructions"] != serverInstructions || contract["tool_json"] != nil {
				t.Fatal("instructions-only request included wrong bodies")
			}
		}
	}
}

// Mutation guard: contract validation errors must be returned before ordinary
// runtime diagnostics can turn an unknown tool/file-like name into success.
func TestInfoContractRejectsUnknownNames(t *testing.T) {
	cs := contractTestClient(t)
	for _, name := range []string{"agy_run_typo", "../../auth.json"} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyInfo, Arguments: map[string]any{"tool": name}})
		if err != nil || !res.IsError {
			t.Fatalf("unknown name unexpectedly succeeded: %+v, %v", res, err)
		}
	}
}
