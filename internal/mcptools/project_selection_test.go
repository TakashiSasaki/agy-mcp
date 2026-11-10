package mcptools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Exercise the registered tool over MCP's newline-delimited JSON transport.
func TestSelectProjectOverMCP(t *testing.T) {
	for _, test := range []struct {
		name    string
		action  string
		status  string
		urlOnly bool
	}{
		{name: "chat fallback", status: "selection_required"},
		{name: "URL-only fallback", status: "selection_required", urlOnly: true},
		{name: "no matches", status: "no_matches"},
		{name: "form choice", action: "accept", status: "selected"},
		{name: "decline", action: "decline", status: "declined"},
		{name: "cancel", action: "cancel", status: "cancelled"},
		{name: "removed while choosing", action: "accept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registrations := projectSelectionRegistrations(t)
			mgr := testManager(t)
			mgr.SetProjectDirsForTest(registrations, t.TempDir())

			query := "Shared Name"
			if test.status == "no_matches" {
				query = "local-c"
			}
			opts := &mcp.ClientOptions{}
			switch {
			case test.urlOnly:
				opts.Capabilities = &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}}
			case test.action != "" || test.status == "no_matches":
				opts.ElicitationHandler = answerProjectForm(t, test.action, filepath.Join(registrations, "cloud-b.json"), test.status == "")
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
			cs := connectTestServer(t, NewServer(mgr), client)
			if got := cs.InitializeResult().ProtocolVersion; got != "2026-07-28" {
				t.Fatalf("negotiated version = %s", got)
			}
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgySelectProject, Arguments: map[string]any{"query": query}})
			if err != nil {
				t.Fatal(err)
			}
			if test.status == "" {
				if !res.IsError || res.StructuredContent != nil {
					t.Fatalf("stale selection should fail: %+v", res)
				}
				return
			}
			out := structuredMap(t, res)
			if res.IsError || out["status"] != test.status {
				t.Fatalf("want %s, got %+v", test.status, out)
			}
			if test.status == "selected" && out["project_id"] != "cloud-b" || test.status != "selected" && out["project_id"] != nil {
				t.Fatalf("unexpected project ID: %+v", out)
			}
			if test.status == "selection_required" {
				assertChatProjectChoices(t, out["candidates"])
			}
			assertSelectedProjectRoots(t, test.status, out["roots"])
		})
	}
}

func answerProjectForm(t *testing.T, action, registration string, remove bool) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	t.Helper()
	return func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		if action == "" {
			t.Error("empty candidate list should not trigger a form")
		}
		assertProjectForm(t, req)
		if remove {
			if err := os.Remove(registration); err != nil {
				return nil, err
			}
		}
		answer := &mcp.ElicitResult{Action: action}
		if action == "accept" {
			answer.Content = map[string]any{"project_id": "cloud-b"}
		}
		return answer, nil
	}
}

func projectSelectionRegistrations(t *testing.T) string {
	t.Helper()
	registrations := t.TempDir()
	for _, project := range []struct {
		id, name string
		roots    int
		local    bool
	}{
		{id: "cloud-a", name: "Shared Name", roots: 2},
		{id: "cloud-b", name: "Shared Name", roots: 1},
		{id: "local-c", name: "Shared Name", roots: 1, local: true},
		{id: "cloud-d", name: "Other Name", roots: 1},
	} {
		resources := make([]map[string]string, 0, project.roots)
		for range project.roots {
			resources = append(resources, map[string]string{"folderUri": t.TempDir()})
		}
		data, err := json.Marshal(map[string]any{
			"id": project.id, "name": project.name, "isWorkspaceOnly": project.local,
			"projectResources": map[string]any{"resources": resources},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(registrations, project.id+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return registrations
}

func assertChatProjectChoices(t *testing.T, value any) {
	t.Helper()
	var candidates []projectChoice
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &candidates); err != nil || len(candidates) != 2 || candidates[0].Label == candidates[1].Label {
		t.Fatalf("expected two distinct labels for cloud projects: %s (%v)", data, err)
	}
	ids := []string{candidates[0].ID, candidates[1].ID}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"cloud-a", "cloud-b"}) {
		t.Fatalf("unexpected candidate IDs: %v", ids)
	}
	for _, candidate := range candidates {
		want := 1
		if candidate.ID == "cloud-a" {
			want = 2
		}
		if len(candidate.Roots) != want {
			t.Errorf("want %d roots for %s, got %v", want, candidate.ID, candidate.Roots)
		}
	}
}

func assertSelectedProjectRoots(t *testing.T, status string, roots any) {
	t.Helper()
	if status != "selected" {
		if roots != nil {
			t.Errorf("unexpected selected project roots with status %s: %v", status, roots)
		}
		return
	}
	list, ok := roots.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("expected one root for the chosen project: %v", roots)
	}
	if root, ok := list[0].(string); !ok || !filepath.IsAbs(root) {
		t.Errorf("expected an absolute server root: %v", list[0])
	}
}

func TestCloudProjectChoicesRetainAllRootsForFolderQuery(t *testing.T) {
	base := t.TempDir()
	matched := filepath.Join(base, "matching-folder")
	other := filepath.Join(base, "other-folder")
	choices := cloudProjectChoices([]manager.ProjectInfo{
		{ID: "project-a", Name: "A", Root: matched},
		{ID: "project-a", Name: "A", Root: other},
		{ID: "project-a", Name: "A", Root: other},
		{ID: "project-b", Name: "B", Root: other},
	}, filepath.Base(matched))
	if len(choices) != 1 || choices[0].ID != "project-a" || !slices.Equal(choices[0].Roots, []string{matched, other}) {
		t.Fatalf("expected the matching project with both distinct roots: %+v", choices)
	}
}

func assertProjectForm(t *testing.T, req *mcp.ElicitRequest) {
	t.Helper()
	data, err := json.Marshal(req.Params.RequestedSchema)
	if err != nil {
		t.Errorf("marshal form: %v", err)
		return
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Errorf("decode form: %v", err)
		return
	}
	field := schema.Properties["project_id"]
	if req.Params.Mode != "form" || !slices.Contains(schema.Required, "project_id") || field == nil || len(field.OneOf) != 2 {
		t.Errorf("expected a required choice between two cloud project IDs: %s", data)
		return
	}
	for _, choice := range field.OneOf {
		if choice.Const == nil || !strings.HasPrefix(choice.Title, "Shared Name (") {
			t.Errorf("expected titled project ID: %s", data)
		}
	}
}
