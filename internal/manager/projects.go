package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProjectWorktree models a linked worktree or checkout associated with a project.
type ProjectWorktree struct {
	Path    string `json:"path" jsonschema:"absolute path to the active linked worktree directory"`
	Branch  string `json:"branch,omitempty" jsonschema:"active git branch name for this worktree"`
	Head    string `json:"head,omitempty" jsonschema:"short commit SHA of the current HEAD"`
	IsClean bool   `json:"is_clean" jsonschema:"true if worktree has no uncommitted changes"`
}

// ProjectInfo provides structured metadata for a repository/project.
type ProjectInfo struct {
	ID            string            `json:"id" jsonschema:"unique project identifier"`
	Name          string            `json:"name" jsonschema:"human-readable project name"`
	Root          string            `json:"root" jsonschema:"root directory of the project container or repository"`
	RepositoryURL string            `json:"repository_url,omitempty" jsonschema:"git remote fetch URL if configured"`
	DefaultBranch string            `json:"default_branch,omitempty" jsonschema:"default or main branch name"`
	Worktrees     []ProjectWorktree `json:"worktrees" jsonschema:"list of active linked worktrees and working branches"`
}

// ResolveWorkspaceResult carries a path only for a unique selection. Resolved
// and Ambiguous are mutually exclusive; candidates never authorize execution.
type ResolveWorkspaceResult struct {
	Resolved   bool     `json:"resolved"`
	Path       string   `json:"path,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Head       string   `json:"head,omitempty"`
	Ambiguous  bool     `json:"ambiguous"`
	Candidates []string `json:"candidates,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func defaultProjectsConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "config", "projects")
}

// ListProjects discovers registered Antigravity projects from ~/.gemini/config/projects/*.json
// and canonical repositories under /data.
func (m *Manager) ListProjects(query string) ([]ProjectInfo, error) {
	projDir := m.projectsDir
	if projDir == "" {
		projDir = defaultProjectsConfigDir()
	}
	dataDir := m.dataDir
	if dataDir == "" {
		dataDir = "/data"
	}

	projects := make(map[string]*ProjectInfo)

	// 1. Scan Antigravity project config files
	if projDir != "" {
		entries, err := os.ReadDir(projDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				pPath := filepath.Join(projDir, entry.Name())
				data, err := os.ReadFile(pPath)
				if err != nil {
					continue
				}
				var pDef struct {
					ID               string `json:"id"`
					Name             string `json:"name"`
					ProjectResources struct {
						Resources []struct {
							FolderURI string `json:"folderUri"`
							GitFolder struct {
								FolderURI     string `json:"folderUri"`
								DefaultBranch string `json:"defaultBranch"`
							} `json:"gitFolder"`
						} `json:"resources"`
					} `json:"projectResources"`
				}
				if err := json.Unmarshal(data, &pDef); err != nil {
					continue
				}
				if pDef.ID == "" && pDef.Name == "" {
					continue
				}
				var rootPath, defBranch string
				for _, r := range pDef.ProjectResources.Resources {
					uri := r.GitFolder.FolderURI
					if uri == "" {
						uri = r.FolderURI
					}
					if strings.HasPrefix(uri, "file://") {
						rootPath = strings.TrimPrefix(uri, "file://")
					} else if uri != "" {
						rootPath = uri
					}
					if r.GitFolder.DefaultBranch != "" {
						defBranch = r.GitFolder.DefaultBranch
					}
				}
				if rootPath == "" {
					continue
				}
				cleanRoot := filepath.Clean(rootPath)
				id := pDef.ID
				if id == "" {
					id = filepath.Base(cleanRoot)
				}
				name := pDef.Name
				if name == "" {
					name = filepath.Base(cleanRoot)
				}
				projects[cleanRoot] = &ProjectInfo{
					ID:            id,
					Name:          name,
					Root:          cleanRoot,
					DefaultBranch: defBranch,
				}
			}
		}
	}

	// 2. Scan /data directories for git repositories
	if fi, err := os.Stat(dataDir); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(dataDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				subPath := filepath.Join(dataDir, entry.Name())
				cleanSub := filepath.Clean(subPath)
				bareDir := filepath.Join(cleanSub, ".bare")
				gitDir := filepath.Join(cleanSub, ".git")

				isGit := false
				if s, err := os.Stat(bareDir); err == nil && s.IsDir() {
					isGit = true
				} else if _, err := os.Stat(gitDir); err == nil {
					isGit = true
				}

				if isGit {
					if _, exists := projects[cleanSub]; !exists {
						projects[cleanSub] = &ProjectInfo{
							ID:   entry.Name(),
							Name: entry.Name(),
							Root: cleanSub,
						}
					}
				}
			}
		}
	}

	// 3. Inspect Git remotes and worktrees for each project
	var out []ProjectInfo
	for _, proj := range projects {
		remoteURL, defBranch, wts := inspectProjectGit(proj.Root)
		if remoteURL != "" {
			proj.RepositoryURL = remoteURL
		}
		if proj.DefaultBranch == "" && defBranch != "" {
			proj.DefaultBranch = defBranch
		}
		if wts == nil {
			wts = []ProjectWorktree{}
		}
		proj.Worktrees = wts

		// If a query is provided, filter by Name, ID, or RepositoryURL
		if query != "" {
			q := strings.ToLower(query)
			nameMatch := strings.Contains(strings.ToLower(proj.Name), q)
			idMatch := strings.Contains(strings.ToLower(proj.ID), q)
			repoMatch := strings.Contains(strings.ToLower(proj.RepositoryURL), q)
			baseMatch := strings.Contains(strings.ToLower(filepath.Base(proj.Root)), q)
			if !nameMatch && !idMatch && !repoMatch && !baseMatch {
				continue
			}
		}
		out = append(out, *proj)
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	if out == nil {
		out = []ProjectInfo{}
	}
	return out, nil
}

func inspectProjectGit(root string) (remoteURL, defBranch string, worktrees []ProjectWorktree) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bareDir := filepath.Join(root, ".bare")
	gitDir := filepath.Join(root, ".git")

	targetGitDir := ""
	isBare := false
	if s, err := os.Stat(bareDir); err == nil && s.IsDir() {
		targetGitDir = bareDir
		isBare = true
	} else if _, err := os.Stat(gitDir); err == nil {
		targetGitDir = gitDir
	}

	if targetGitDir == "" {
		return "", "", nil
	}

	// Remote URL
	cmdRemote := probeCmd(ctx, "git", "--git-dir="+targetGitDir, "remote", "-v")
	if out, err := cmdRemote.Output(); err == nil {
		remotes := parseGitRemotes(string(out))
		if u, ok := remotes["github"]; ok {
			remoteURL = u
		} else if u, ok := remotes["origin"]; ok {
			remoteURL = u
		} else {
			for _, u := range remotes {
				remoteURL = u
				break
			}
		}
	}

	// Worktrees
	if isBare {
		cmdWt := probeCmd(ctx, "git", "--git-dir="+targetGitDir, "worktree", "list", "--porcelain")
		if out, err := cmdWt.Output(); err == nil {
			worktrees = parseWorktreeListPorcelain(string(out), targetGitDir)
		}
	} else {
		// Single non-bare checkout
		if s, err := os.Stat(root); err == nil && s.IsDir() {
			branch, head, dirty := DetectGitMetadata(root)
			isClean := dirty != nil && !*dirty
			worktrees = []ProjectWorktree{
				{
					Path:    root,
					Branch:  branch,
					Head:    head,
					IsClean: isClean,
				},
			}
		}
	}

	// Find default branch
	for _, wt := range worktrees {
		if wt.Branch == "main" || wt.Branch == "master" {
			defBranch = wt.Branch
			break
		}
	}
	if defBranch == "" && len(worktrees) > 0 {
		defBranch = worktrees[0].Branch
	}

	return remoteURL, defBranch, worktrees
}

func parseGitRemotes(output string) map[string]string {
	remotes := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[2] == "(fetch)" {
			remotes[parts[0]] = parts[1]
		}
	}
	return remotes
}

func parseWorktreeListPorcelain(output, bareDir string) []ProjectWorktree {
	var worktrees []ProjectWorktree
	lines := strings.Split(output, "\n")
	var curPath, curHead, curBranch string
	var isBare bool

	flush := func() {
		if curPath != "" && !isBare && curPath != bareDir {
			cleanPath := filepath.Clean(curPath)
			if fi, err := os.Stat(cleanPath); err == nil && fi.IsDir() {
				_, _, dirty := DetectGitMetadata(cleanPath)
				isClean := dirty != nil && !*dirty
				branch := curBranch
				if strings.HasPrefix(branch, "refs/heads/") {
					branch = strings.TrimPrefix(branch, "refs/heads/")
				}
				head := curHead
				if len(head) > 7 {
					head = head[:7]
				}
				worktrees = append(worktrees, ProjectWorktree{
					Path:    cleanPath,
					Branch:  branch,
					Head:    head,
					IsClean: isClean,
				})
			}
		}
		curPath, curHead, curBranch = "", "", ""
		isBare = false
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		if line == "bare" {
			isBare = true
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			curPath = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "HEAD ") {
			curHead = strings.TrimPrefix(line, "HEAD ")
		} else if strings.HasPrefix(line, "branch ") {
			curBranch = strings.TrimPrefix(line, "branch ")
		}
	}
	flush()
	return worktrees
}

// ResolveWorkspace selects a unique working directory without guessing a
// default when multiple worktrees match. An explicit directory pins the path;
// an accompanying branch must match that directory's branch or folder alias.
func (m *Manager) ResolveWorkspace(projectQuery, branchQuery string) (ResolveWorkspaceResult, error) {
	if projectQuery == "" {
		return ResolveWorkspaceResult{Resolved: false, Error: "project query is required"}, nil
	}

	// 1. Direct path check
	cleanPath := filepath.Clean(projectQuery)
	if filepath.IsAbs(cleanPath) {
		return resolveWorkspaceDirectory(cleanPath, branchQuery), nil
	}

	// 2. List projects and match
	projects, err := m.ListProjects("")
	if err != nil {
		return ResolveWorkspaceResult{}, err
	}

	var matched []ProjectInfo
	pq := strings.ToLower(projectQuery)

	// Strategy A: Exact ID match
	for _, p := range projects {
		if strings.EqualFold(p.ID, projectQuery) {
			matched = append(matched, p)
		}
	}
	// Strategy B: Exact Name match
	if len(matched) == 0 {
		for _, p := range projects {
			if strings.EqualFold(p.Name, projectQuery) {
				matched = append(matched, p)
			}
		}
	}
	// Strategy C: Exact Base folder match
	if len(matched) == 0 {
		for _, p := range projects {
			if strings.EqualFold(filepath.Base(p.Root), projectQuery) {
				matched = append(matched, p)
			}
		}
	}
	// Strategy D: Substring match
	if len(matched) == 0 {
		for _, p := range projects {
			if strings.Contains(strings.ToLower(p.Name), pq) || strings.Contains(strings.ToLower(p.RepositoryURL), pq) {
				matched = append(matched, p)
			}
		}
	}

	if len(matched) == 0 {
		return ResolveWorkspaceResult{
			Resolved: false,
			Error:    fmt.Sprintf("no project found matching %q", projectQuery),
		}, nil
	}

	if len(matched) > 1 {
		var candNames []string
		for _, p := range matched {
			candNames = append(candNames, fmt.Sprintf("%s (%s)", p.Name, p.Root))
		}
		return ResolveWorkspaceResult{
			Resolved:   false,
			Ambiguous:  true,
			Candidates: candNames,
			Error:      fmt.Sprintf("multiple projects matched %q, be more specific", projectQuery),
		}, nil
	}

	return resolveProjectWorktree(matched[0], branchQuery), nil
}

func resolveWorkspaceDirectory(path, branchQuery string) ResolveWorkspaceResult {
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return ResolveWorkspaceResult{Error: fmt.Sprintf("workspace directory %q does not exist or is unavailable", path)}
	}
	branch, head, _ := DetectGitMetadata(path)
	if branchQuery != "" && !strings.EqualFold(branch, branchQuery) && !strings.EqualFold(filepath.Base(path), branchQuery) {
		return ResolveWorkspaceResult{Error: fmt.Sprintf("branch %q does not match directory %q (branch %q)", branchQuery, path, branch)}
	}
	return ResolveWorkspaceResult{Resolved: true, Path: path, Branch: branch, Head: head}
}

func worktreeCandidates(worktrees []ProjectWorktree) []string {
	paths := make([]string, 0, len(worktrees))
	for _, wt := range worktrees {
		paths = append(paths, fmt.Sprintf("%s [%s]", wt.Path, wt.Branch))
	}
	return paths
}

func resolveProjectWorktree(proj ProjectInfo, branchQuery string) ResolveWorkspaceResult {
	if len(proj.Worktrees) == 0 && branchQuery == "" {
		return resolveWorkspaceDirectory(proj.Root, "")
	}
	matches := proj.Worktrees
	if branchQuery != "" {
		matches = nil
		// Full, case-sensitive Git branch names outrank convenience aliases.
		for _, wt := range proj.Worktrees {
			if wt.Branch == branchQuery {
				matches = append(matches, wt)
			}
		}
		if len(matches) == 0 {
			for _, wt := range proj.Worktrees {
				if strings.EqualFold(wt.Branch, branchQuery) || strings.EqualFold(filepath.Base(wt.Path), branchQuery) {
					matches = append(matches, wt)
				}
			}
		}
	}
	switch len(matches) {
	case 0:
		return ResolveWorkspaceResult{Candidates: worktreeCandidates(proj.Worktrees),
			Error: fmt.Sprintf("branch %q not found in project %q; choose an available branch or path", branchQuery, proj.Name)}
	case 1:
		wt := matches[0]
		// Recheck existence instead of returning a path that vanished since discovery.
		out := resolveWorkspaceDirectory(wt.Path, "")
		if out.Resolved {
			out.Branch, out.Head = wt.Branch, wt.Head
		}
		return out
	default:
		return ResolveWorkspaceResult{Ambiguous: true, Candidates: worktreeCandidates(matches),
			Error: fmt.Sprintf("multiple worktrees match project %q and branch %q; specify an exact branch or absolute path", proj.Name, branchQuery)}
	}
}
