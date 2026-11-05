package manager

import (
	"os"
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
	root := t.TempDir()
	paths := []string{filepath.Join(root, "literal"), filepath.Join(root, "feature", "review"), filepath.Join(root, "hotfix", "review")}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	project := ProjectInfo{Name: "aliases", Worktrees: []ProjectWorktree{
		{Path: paths[1], Branch: "feature/review"},
		{Path: paths[0], Branch: "review"},
		{Path: paths[2], Branch: "hotfix/review"},
	}}
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
