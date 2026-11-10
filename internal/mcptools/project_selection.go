package mcptools

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const projectSelectionGuide = "If the request or conversation identifies one cloud project, run with its ID or unique name. Otherwise call agy_select_project and wait for the user's choice. If it returns candidates, show them in chat."

const projectSelectionField = "project_id"

type selectProjectInput struct {
	Query string `json:"query,omitempty" jsonschema:"optional substring to narrow the cloud projects by name, ID, repository URL, or folder name"`
}

type projectChoice struct {
	ID    string   `json:"id" jsonschema:"registered cloud project ID to pass as project to a run tool"`
	Label string   `json:"label" jsonschema:"display name for the user, with identifying details when names are shared"`
	Roots []string `json:"roots" jsonschema:"registered root directories on the server for this project; use as workspace hints"`
}

type selectProjectOutput struct {
	Status     string          `json:"status" jsonschema:"selected when the user chose a project, selection_required when the user must choose in chat, no_matches when no cloud project matches, or declined or cancelled when the user did not choose"`
	ProjectID  string          `json:"project_id,omitempty" jsonschema:"chosen project ID to pass as project to a run tool; present only when selected"`
	Roots      []string        `json:"roots,omitempty" jsonschema:"registered root directories of the chosen project; use agy_resolve_workspace if the execution directory is unclear"`
	Candidates []projectChoice `json:"candidates,omitempty" jsonschema:"choices to show the user when selection is required in chat; wait for an answer before running"`
}

func selectCloudProject(mgr *manager.Manager, req *mcp.CallToolRequest, in selectProjectInput) (*mcp.CallToolResult, selectProjectOutput, error) {
	var selectedID string
	if len(req.Params.InputResponses) > 0 {
		answer, ok := req.Params.InputResponses[projectSelectionField].(*mcp.ElicitResult)
		if !ok || answer == nil {
			return nil, selectProjectOutput{}, fmt.Errorf("missing project selection response")
		}
		switch answer.Action {
		case "decline":
			return nil, selectProjectOutput{Status: "declined"}, nil
		case "cancel":
			return nil, selectProjectOutput{Status: "cancelled"}, nil
		case "accept":
			selectedID, _ = answer.Content[projectSelectionField].(string)
			if selectedID == "" {
				return nil, selectProjectOutput{}, fmt.Errorf("missing selected project ID")
			}
		default:
			return nil, selectProjectOutput{}, fmt.Errorf("invalid project selection action")
		}
	}

	projects, err := mgr.ListProjects("", "cloud_only")
	if err != nil {
		return nil, selectProjectOutput{}, err
	}
	choices := cloudProjectChoices(projects, strings.TrimSpace(in.Query))
	if selectedID != "" {
		for _, choice := range choices {
			if choice.ID == selectedID {
				return nil, selectProjectOutput{Status: "selected", ProjectID: choice.ID, Roots: choice.Roots}, nil
			}
		}
		return nil, selectProjectOutput{}, fmt.Errorf("selected project is no longer available for this query; call agy_select_project again")
	}
	if len(choices) == 0 {
		return nil, selectProjectOutput{Status: "no_matches"}, nil
	}
	// The titled enum used by the form was added in MCP 2025-11-25.
	if caps := req.ClientCapabilities(); req.ProtocolVersion() >= "2025-11-25" && caps != nil && caps.Elicitation != nil {
		// An empty elicitation capability denotes form support; URL-only does not.
		if caps.Elicitation.Form != nil || caps.Elicitation.URL == nil {
			return projectChoiceForm(choices), selectProjectOutput{}, nil
		}
	}
	return nil, selectProjectOutput{Status: "selection_required", Candidates: choices}, nil
}

func cloudProjectChoices(projects []manager.ProjectInfo, query string) []projectChoice {
	// The registry exposes one row per root. Selection identifies the project,
	// so keep all roots for each matching ID, even when only one root matched.
	roots := make(map[string][]string)
	seen := make(map[string]bool)
	nameCount := make(map[string]int)
	unique := make([]*manager.ProjectInfo, 0, len(projects))
	for i := range projects {
		project := &projects[i]
		if !slices.Contains(roots[project.ID], project.Root) {
			roots[project.ID] = append(roots[project.ID], project.Root)
		}
		if !seen[project.ID] && project.MatchesQuery(query) {
			seen[project.ID] = true
			nameCount[strings.ToLower(project.Name)]++
			unique = append(unique, project)
		}
	}
	choices := make([]projectChoice, 0, len(unique))
	for _, project := range unique {
		label := project.Name
		if nameCount[strings.ToLower(project.Name)] > 1 {
			label = fmt.Sprintf("%s (%s, %s)", project.Name, project.Root, project.ID)
		}
		choices = append(choices, projectChoice{ID: project.ID, Label: label, Roots: roots[project.ID]})
	}
	return choices
}

func projectChoiceForm(choices []projectChoice) *mcp.CallToolResult {
	options := make([]*jsonschema.Schema, 0, len(choices))
	for _, choice := range choices {
		options = append(options, &jsonschema.Schema{Const: new(any(choice.ID)), Title: choice.Label})
	}
	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{
		projectSelectionField: &mcp.ElicitParams{
			Mode:    "form",
			Message: "Which Antigravity cloud project should receive this job's conversation?",
			RequestedSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					projectSelectionField: {Type: "string", Title: "Cloud project", OneOf: options},
				},
				Required: []string{projectSelectionField},
			},
		},
	}}
}
