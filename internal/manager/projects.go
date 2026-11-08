package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ProjectWorktree models a linked worktree or checkout associated with a project.
type ProjectWorktree struct {
	HeadFull string `json:"head_full,omitempty" jsonschema:"full Git commit object ID for expected_git_head preconditions; absent if unavailable"`
	Path     string `json:"path" jsonschema:"absolute path to the active linked worktree directory"`
	Branch   string `json:"branch,omitempty" jsonschema:"active git branch name for this worktree"`
	Head     string `json:"head,omitempty" jsonschema:"short commit SHA of the current HEAD"`
	IsClean  bool   `json:"is_clean" jsonschema:"true if Git reports no tracked changes; untracked files are ignored"`
}

// ProjectInfo provides structured metadata for a repository/project.
type ProjectInfo struct {
	ID            string            `json:"id" jsonschema:"project identifier; a multi-root project has one entry per root with the same identifier"`
	Name          string            `json:"name" jsonschema:"human-readable project name"`
	Root          string            `json:"root" jsonschema:"root directory of the project container or repository"`
	RepositoryURL string            `json:"repository_url,omitempty" jsonschema:"git remote fetch URL if configured"`
	DefaultBranch string            `json:"default_branch,omitempty" jsonschema:"default or main branch name"`
	Worktrees     []ProjectWorktree `json:"worktrees" jsonschema:"list of active linked worktrees and working branches"`
}

// ResolveWorkspaceResult carries a path only for a unique selection. Resolved
// and Ambiguous are mutually exclusive; candidates never authorize execution.
type ResolveWorkspaceResult struct {
	HeadFull   string   `json:"head_full,omitempty"`
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

	projects, registeredRoots := discoverRegisteredProjects(projDir)
	discoverDataProjects(dataDir, projects, registeredRoots)

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
		a, b := out[i], out[j]
		lowerA, lowerB := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if lowerA != lowerB {
			return lowerA < lowerB
		}
		if a.Root != b.Root {
			return a.Root < b.Root
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Name < b.Name
	})

	if out == nil {
		out = []ProjectInfo{}
	}
	return out, nil
}

type projectKey struct{ id, name, root string }

type projectRegistration struct {
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

func discoverRegisteredProjects(dir string) (projects map[projectKey]*ProjectInfo, roots map[string]bool) {
	projects = make(map[projectKey]*ProjectInfo)
	roots = make(map[string]bool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return projects, roots
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var registration projectRegistration
		if err := json.Unmarshal(data, &registration); err != nil || (registration.ID == "" && registration.Name == "") {
			continue
		}
		for _, resource := range registration.ProjectResources.Resources {
			uri := resource.GitFolder.FolderURI
			if uri == "" {
				uri = resource.FolderURI
			}
			root, ok := projectResourcePath(uri)
			if !ok {
				continue
			}
			id, name := registration.ID, registration.Name
			if id == "" {
				id = filepath.Base(root)
			}
			if name == "" {
				name = filepath.Base(root)
			}
			// Keep each resource and registration identity for ambiguity checks.
			key := projectKey{id, name, root}
			projects[key] = &ProjectInfo{ID: id, Name: name, Root: root,
				DefaultBranch: resource.GitFolder.DefaultBranch}
			roots[root] = true
		}
	}
	return projects, roots
}

func discoverDataProjects(dir string, projects map[projectKey]*ProjectInfo, registeredRoots map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(dir, entry.Name())
		if registeredRoots[root] || projectGitDir(root) == "" {
			continue
		}
		name := entry.Name()
		projects[projectKey{name, name, root}] = &ProjectInfo{ID: name, Name: name, Root: root}
	}
}

func projectGitDir(root string) string {
	bareDir := filepath.Join(root, ".bare")
	if info, err := os.Stat(bareDir); err == nil && info.IsDir() {
		return bareDir
	}
	gitDir := filepath.Join(root, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return gitDir
	}
	return ""
}

// projectResourcePath accepts absolute local paths and local file URIs only.
// URL.Path decodes percent escapes once, preserving literal percent sequences.
func projectResourcePath(resource string) (string, bool) {
	path := resource
	if strings.HasPrefix(strings.ToLower(resource), "file:") {
		u, err := url.Parse(resource)
		if err != nil || u.Opaque != "" || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		path = filepath.FromSlash(u.Path)
		// A Windows file URI uses /C:/..., while native paths use C:\\... .
		if len(path) > 1 && os.IsPathSeparator(path[0]) && filepath.VolumeName(path[1:]) != "" {
			path = path[1:]
		}
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", false
	}
	return filepath.Clean(path), true
}

func inspectProjectGit(root string) (remoteURL, defBranch string, worktrees []ProjectWorktree) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	targetGitDir := projectGitDir(root)
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

	// Both ordinary and shared-bare repositories can own linked worktrees.
	cmdWt := probeCmd(ctx, "git", "--git-dir="+targetGitDir, "worktree", "list", "--porcelain", "-z")
	if out, err := cmdWt.Output(); err == nil {
		worktrees = parseWorktreeList(string(out), targetGitDir)
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
	for line := range strings.Lines(output) {
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[2] == "(fetch)" {
			remotes[parts[0]] = parts[1]
		}
	}
	return remotes
}

func parseWorktreeList(output, bareDir string) []ProjectWorktree {
	var worktrees []ProjectWorktree
	var curPath, curHead, curBranch string
	var isBare bool

	flush := func() {
		if curPath != "" && !isBare && curPath != bareDir {
			cleanPath := filepath.Clean(curPath)
			if fi, err := os.Stat(cleanPath); err == nil && fi.IsDir() {
				var head, fullHead string
				isClean := false
				if validExpectedGitHead(curHead) && strings.Trim(curHead, "0") != "" {
					head, fullHead = curHead[:7], curHead
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					dirty := gitDirty(ctx, cleanPath)
					cancel()
					isClean = dirty != nil && !*dirty
				}
				branch := strings.TrimPrefix(curBranch, "refs/heads/")
				worktrees = append(worktrees, ProjectWorktree{
					Path:     cleanPath,
					HeadFull: fullHead,
					Branch:   branch,
					Head:     head,
					IsClean:  isClean,
				})
			}
		}
		curPath, curHead, curBranch = "", "", ""
		isBare = false
	}

	for line := range strings.SplitSeq(output, "\x00") {
		if line == "" {
			flush()
			continue
		}
		if line == "bare" {
			isBare = true
			continue
		}
		field, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch field {
		case "worktree":
			curPath = value
		case "HEAD":
			curHead = value
		case "branch":
			curBranch = value
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
		slices.Sort(candNames)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// An unborn branch has a symbolic ref before it has a commit.
	branch, _ := readGitBranch(ctx, path)
	var head string
	if branchQuery != "" && !strings.EqualFold(branch, branchQuery) && !strings.EqualFold(filepath.Base(path), branchQuery) {
		return ResolveWorkspaceResult{Error: fmt.Sprintf("branch %q does not match directory %q (branch %q)", branchQuery, path, branch)}
	}
	fullHead, _ := readGitHead(ctx, path)
	if fullHead != "" {
		if out, err := probeCmd(ctx, "git", "-C", path, "rev-parse", "--short", fullHead).Output(); err == nil {
			head = strings.TrimSpace(string(out))
		}
	}
	return ResolveWorkspaceResult{Resolved: true, Path: path, Branch: branch, Head: head, HeadFull: fullHead}
}

func worktreeCandidates(worktrees []ProjectWorktree) []string {
	paths := make([]string, 0, len(worktrees))
	for _, wt := range worktrees {
		paths = append(paths, fmt.Sprintf("%s [%s]", wt.Path, wt.Branch))
	}
	slices.Sort(paths)
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
		out := resolveWorkspaceDirectory(wt.Path, "")
		if out.Resolved {
			if out.Branch != wt.Branch || out.HeadFull != wt.HeadFull {
				return ResolveWorkspaceResult{Error: fmt.Sprintf("workspace %q changed branch or HEAD during discovery; resolve it again", wt.Path)}
			}
			out.Head = wt.Head
		}
		return out
	default:
		return ResolveWorkspaceResult{Ambiguous: true, Candidates: worktreeCandidates(matches),
			Error: fmt.Sprintf("multiple worktrees match project %q and branch %q; specify an exact branch or absolute path", proj.Name, branchQuery)}
	}
}
