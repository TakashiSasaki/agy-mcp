package manager

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func linkedProjectFixture(t *testing.T) (mgr *Manager, projectRoot string, pathsByBranch map[string]string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	createTestGitRepo(t, seed)
	data := filepath.Join(root, "data")
	project := filepath.Join(data, "linked-project")
	bare := filepath.Join(project, ".bare")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("clone", "--bare", seed, bare)
	paths := make(map[string]string)
	for _, branch := range []string{"main", "feature/review", "hotfix/review"} {
		path := filepath.Join(project, filepath.FromSlash(branch))
		args := []string{"--git-dir=" + bare, "worktree", "add"}
		if branch != "main" {
			args = append(args, "-b", branch)
		}
		args = append(args, path, "main")
		run(args...)
		paths[branch] = path
	}
	return &Manager{projectsDir: t.TempDir(), dataDir: data}, project, paths
}

// Mutation guards: resolveProjectWorktree must refuse multiple candidates,
// including a default main worktree, and must not choose the first alias.
// Direct paths must honor their branch constraint instead of ignoring it.
func TestResolveWorkspaceRequiresUniqueWorktree(t *testing.T) {
	mgr, _, paths := linkedProjectFixture(t)
	for _, tc := range []struct {
		name, project, branch, wantPath string
		ambiguous                       bool
		candidates                      int
	}{
		{"default branch is not implicit", "linked-project", "", "", true, 3},
		{"exact main", "linked-project", "main", paths["main"], false, 0},
		{"exact nested branch", "linked-project", "feature/review", paths["feature/review"], false, 0},
		{"colliding folder aliases", "linked-project", "review", "", true, 2},
		{"missing branch", "linked-project", "missing", "", false, 3},
		{"explicit path", paths["main"], "", paths["main"], false, 0},
		{"path branch matches", paths["main"], "main", paths["main"], false, 0},
		{"path branch mismatch", paths["main"], "feature/review", "", false, 0},
		{"missing absolute directory", filepath.Join(t.TempDir(), "missing"), "", "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mgr.ResolveWorkspace(tc.project, tc.branch)
			if err != nil {
				t.Fatal(err)
			}
			if out.Resolved != (tc.wantPath != "") || out.Ambiguous != tc.ambiguous || out.Path != tc.wantPath || len(out.Candidates) != tc.candidates {
				t.Fatalf("unsafe resolution: %+v, want path %q ambiguous %t candidates %d", out, tc.wantPath, tc.ambiguous, tc.candidates)
			}
			if tc.wantPath == "" && (out.Branch != "" || out.Head != "" || out.Error == "") {
				t.Fatalf("unresolved selection leaked execution metadata or lost advice: %+v", out)
			}
		})
	}
}

// Mutation guard: full Git branch matches must precede case/folder aliases,
// and a single vanished worktree must not be reported resolved.
func TestWorkspaceExactBranchPrecedesAliases(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "literal"), filepath.Join(root, "feature", "review"), filepath.Join(root, "hotfix", "review")}
	branches := []string{"review", "feature/review", "hotfix/review"}
	project := ProjectInfo{Name: "aliases"}
	for _, i := range []int{1, 0, 2} {
		path := paths[i]
		createTestGitRepo(t, path)
		workspaceGit(t, path, "switch", "-c", branches[i])
		_, _, trees := inspectProjectGit(path)
		if len(trees) != 1 {
			t.Fatalf("invalid worktree fixture for %q: %+v", path, trees)
		}
		project.Worktrees = append(project.Worktrees, trees[0])
	}
	out := resolveProjectWorktree(project, "review")
	if !out.Resolved || out.Ambiguous || out.Path != paths[0] || out.Branch != "review" {
		t.Fatalf("exact branch did not outrank earlier aliases: %+v", out)
	}
	out = resolveProjectWorktree(project, "REVIEW")
	if out.Resolved || !out.Ambiguous || out.Path != "" || len(out.Candidates) != 3 {
		t.Fatalf("case-insensitive aliases silently picked a worktree: %+v", out)
	}
	project.Worktrees = []ProjectWorktree{{Path: filepath.Join(root, "gone"), Branch: "main"}}
	out = resolveProjectWorktree(project, "")
	if out.Resolved || out.Ambiguous || out.Path != "" || out.Error == "" {
		t.Fatalf("vanished directory was resolved: %+v", out)
	}
}

// Mutation guard: Git emits a zero HEAD in porcelain output for an unborn
// branch. The branch is selectable, but it has no commit to pin.
func TestResolveWorkspaceUnbornBranchHasNoHead(t *testing.T) {
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceGit(t, path, "init", "-b", "main")
	projects := t.TempDir()
	writeProjectResources(t, projects, "unborn.json", "unborn", "Unborn", path)
	m := &Manager{projectsDir: projects, dataDir: t.TempDir()}
	projs, err := m.ListProjects("")
	if err != nil || len(projs) != 1 || len(projs[0].Worktrees) != 1 {
		t.Fatalf("unborn project listing: %+v, %v", projs, err)
	}
	if wt := projs[0].Worktrees[0]; wt.Branch != "main" || wt.Head != "" || wt.HeadFull != "" {
		t.Errorf("unborn branch advertised a commit: %+v", wt)
	}
	for _, query := range []string{"unborn", path} {
		got, err := m.ResolveWorkspace(query, "main")
		if err != nil || !got.Resolved || got.Ambiguous || got.Path != path || got.Branch != "main" || got.Head != "" || got.HeadFull != "" {
			t.Errorf("unborn branch %q: %+v, %v", query, got, err)
		}
	}
	plain := t.TempDir()
	got, err := m.ResolveWorkspace(plain, "")
	if err != nil || !got.Resolved || got.Ambiguous || got.Path != plain || got.Branch != "" || got.Head != "" || got.HeadFull != "" {
		t.Fatalf("plain directory: %+v, %v", got, err)
	}
}

// Mutation guard: resolution must revalidate the selected worktree's Git
// identity, including branch-only and commit-only changes after discovery.
func TestResolveWorkspaceRefusesStaleGitSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"branch", []string{"switch", "-c", "moved"}},
		{"commit", []string{"commit", "--allow-empty", "-m", "moved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checkout")
			createTestGitRepo(t, path)
			_, _, trees := inspectProjectGit(path)
			if len(trees) != 1 || trees[0].Branch != "main" || trees[0].HeadFull == "" {
				t.Fatalf("invalid discovery fixture: %+v", trees)
			}
			proj := ProjectInfo{Name: "test project", Worktrees: trees}
			if got := resolveProjectWorktree(proj, "main"); !got.Resolved || got.HeadFull != trees[0].HeadFull {
				t.Fatalf("stable snapshot did not resolve: %+v", got)
			}
			workspaceGit(t, path, tc.args...)
			got := resolveProjectWorktree(proj, "main")
			if got.Resolved || got.Ambiguous || got.Path != "" || got.Branch != "" || got.Head != "" || got.HeadFull != "" || got.Error == "" {
				t.Fatalf("stale %s was presented as resolved: %+v", tc.name, got)
			}
		})
	}
}

func workspaceGit(t *testing.T, path string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
