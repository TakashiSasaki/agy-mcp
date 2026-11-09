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
	Query      string `json:"query,omitempty" jsonschema:"optional query substring to filter projects by name, id, repository URL, or folder name"`
	SyncFilter string `json:"sync_filter,omitempty" jsonschema:"optional filter by cloud sync status: 'all' (default), 'cloud_only' (synchronized with Antigravity cloud / AgiHub), or 'local_only' (local-only or unregistered repositories)"`
}

type listProjectsOutput struct {
	Projects []manager.ProjectInfo `json:"projects" jsonschema:"list of discovered projects, repositories, and linked worktrees; multi-root projects have one entry per root and may repeat a project ID"`
}

type resolveWorkspaceInput struct {
	Project string `json:"project" jsonschema:"project name, project id, repository URL, or directory path to resolve"`
	Branch  string `json:"branch,omitempty" jsonschema:"optional full Git branch name or worktree folder alias. Exact case-sensitive branch names take precedence; otherwise case-insensitive branch/folder matches must be unique. With an absolute project directory, this verifies that directory's branch or folder alias and never silently ignores a mismatch"`
}

type resolveWorkspaceOutput struct {
	HeadFull   string   `json:"head_full,omitempty" jsonschema:"full Git commit object ID for expected_git_head on agy_run; present only for a uniquely resolved committed Git workspace, absent when unavailable"`
	Resolved   bool     `json:"resolved" jsonschema:"true if a concrete workspace directory was unambiguously resolved"`
	Path       string   `json:"path,omitempty" jsonschema:"execution directory; present only when resolved is true and ambiguous is false; never use candidate descriptions as cwd"`
	Branch     string   `json:"branch,omitempty" jsonschema:"active git branch for the resolved directory"`
	Head       string   `json:"head,omitempty" jsonschema:"short commit SHA for the resolved directory"`
	Ambiguous  bool     `json:"ambiguous" jsonschema:"always present; true means selection is required, resolved is false and path/branch/head are absent. false with resolved false means no matching selection or a missing/unavailable directory"`
	Candidates []string `json:"candidates,omitempty" jsonschema:"candidate workspace paths or branch descriptions when ambiguous"`
	Error      string   `json:"error,omitempty" jsonschema:"error explanation if resolution failed"`
}

func registerProjects(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyListProjects,
		Title:       "List registered projects",
		Annotations: annReadLocal,
		Description: "List registered Antigravity projects and repositories under /data with their paths, git remote URLs, default branches, active linked worktrees, and cloud synchronization status (is_cloud_synced, sync_scope, conversation_count, last_active_time). Every local resource is retained; project IDs can repeat across roots, and distinct registrations sharing a root remain discoverable. Ordinary repositories and shared-bare repositories both enumerate linked worktrees. Local file URIs are decoded; remote file authorities and nonlocal URIs are omitted. Use sync_filter ('cloud_only', 'local_only') to filter by AgiHub cloud synchronization status. Use query to filter by name, ID, or repo URL. Pair with agy_resolve_workspace to resolve a project to a concrete working directory for agy_run.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProjectsInput) (*mcp.CallToolResult, listProjectsOutput, error) {
		projs, err := mgr.ListProjects(in.Query, in.SyncFilter)
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
		Description: "Resolve project (required name, ID, repository URL or server-side absolute directory) and optional branch to a unique cwd for agy_run or agy_run_sync. Only resolved true with ambiguous false carries a usable path. A resolved committed Git workspace includes head_full for agy_run expected_git_head; head retains its short display value. An unborn branch can resolve without either HEAD field. Multiple matches return resolved false, ambiguous true and candidates, with no path/branch/head; a default branch is never selected implicitly. Specify a full branch name or an absolute candidate path, then resolve again. Exact case-sensitive branch names outrank case-insensitive branch/folder aliases; aliases must be unique. For an absolute directory, branch is a constraint on that directory, not a request to switch branches. Missing branches or directories return resolved false and an error, without a fallback path. Resolve again if the selected worktree changes branch or HEAD during discovery. A project registered with multiple roots remains ambiguous by ID; select an absolute root directory. Call agy_list_projects to browse choices. This tool never starts work or changes a checkout.",
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
			HeadFull:   res.HeadFull,
			Ambiguous:  res.Ambiguous,
			Candidates: res.Candidates,
			Error:      res.Error,
		}, nil
	})
}
