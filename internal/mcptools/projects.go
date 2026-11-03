package mcptools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyListProjects     = "agy_list_projects"
	toolAgyResolveWorkspace = "agy_resolve_workspace"
)

type listProjectsInput struct {
	Query string `json:"query,omitempty" jsonschema:"optional query substring to filter projects by name, id, repository URL, or folder name"`
}

type listProjectsOutput struct {
	Projects []manager.ProjectInfo `json:"projects" jsonschema:"list of discovered projects, repositories, and linked worktrees"`
}

type resolveWorkspaceInput struct {
	Project string `json:"project" jsonschema:"project name, project id, repository URL, or directory path to resolve"`
	Branch  string `json:"branch,omitempty" jsonschema:"optional branch name or worktree folder name to resolve within the project"`
}

type resolveWorkspaceOutput struct {
	Resolved   bool     `json:"resolved" jsonschema:"true if a concrete workspace directory was unambiguously resolved"`
	Path       string   `json:"path,omitempty" jsonschema:"resolved concrete filesystem directory path"`
	Branch     string   `json:"branch,omitempty" jsonschema:"active git branch for the resolved directory"`
	Head       string   `json:"head,omitempty" jsonschema:"short commit SHA for the resolved directory"`
	Ambiguous  bool     `json:"ambiguous,omitempty" jsonschema:"true if multiple candidate directories or branches exist"`
	Candidates []string `json:"candidates,omitempty" jsonschema:"candidate workspace paths or branch descriptions when ambiguous"`
	Error      string   `json:"error,omitempty" jsonschema:"error explanation if resolution failed"`
}

func registerProjects(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyListProjects,
		Title:       "List registered projects",
		Annotations: annReadLocal,
		Description: "List registered Antigravity projects and repositories under /data with their paths, git remote URLs, default branches, and active linked worktrees. Use it to discover available projects before starting a run, or pass query to filter by name, ID, or repo URL. Pair with agy_resolve_workspace to resolve a project to a concrete working directory for agy_run.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProjectsInput) (*mcp.CallToolResult, listProjectsOutput, error) {
		projs, err := mgr.ListProjects(in.Query)
		if err != nil {
			return nil, listProjectsOutput{}, err
		}
		if projs == nil {
			projs = []manager.ProjectInfo{}
		}
		return nil, listProjectsOutput{Projects: projs}, nil
	})

	addTool(s, &mcp.Tool{
		Name:        toolAgyResolveWorkspace,
		Title:       "Resolve project workspace",
		Annotations: annReadLocal,
		Description: "Resolve a project name, project ID, repo URL, or directory path and optional branch to an unambiguous concrete working directory path for agy_run or agy_run_sync. If multiple candidate worktrees exist and no branch is given, reports ambiguous with candidates. Call agy_list_projects first if you need to browse registered projects.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resolveWorkspaceInput) (*mcp.CallToolResult, resolveWorkspaceOutput, error) {
		res, err := mgr.ResolveWorkspace(in.Project, in.Branch)
		if err != nil {
			return nil, resolveWorkspaceOutput{}, err
		}
		return nil, resolveWorkspaceOutput{
			Resolved:   res.Resolved,
			Path:       res.Path,
			Branch:     res.Branch,
			Head:       res.Head,
			Ambiguous:  res.Ambiguous,
			Candidates: res.Candidates,
			Error:      res.Error,
		}, nil
	})
}
