package manager

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestGitPreconditionsRequireExactBranchAndFullHead(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	createTestGitRepo(t, repo)
	head, err := readGitHead(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, branch, head string
		pass               bool
	}{
		{"both match", "main", head, true}, {"head only", "", head, true}, {"branch only", "main", "", true},
		{"wrong branch", "other", head, false}, {"branch case is exact", "MAIN", head, false}, {"wrong head", "main", strings.Repeat("0", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGitPreconditions(StartRequest{Cwd: repo, ExpectedGitBranch: tc.branch, ExpectedGitHead: tc.head})
			if tc.pass {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			start, ok := errors.AsType[*StartError](err)
			if !ok || start.Code != "workspace_mismatch" || start.JobStarted == nil || *start.JobStarted {
				t.Fatalf("wrong mismatch: %v", err)
			}
		})
	}
	if out, err := exec.Command("git", "-C", repo, "checkout", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("detach: %v %s", err, out)
	}
	if err := checkGitPreconditions(StartRequest{Cwd: repo, ExpectedGitHead: head}); err != nil {
		t.Fatalf("matching detached commit should pass: %v", err)
	}
	if err := checkGitPreconditions(StartRequest{Cwd: repo, ExpectedGitBranch: "main"}); err == nil {
		t.Fatal("detached HEAD passed branch precondition")
	}
	if err := checkGitPreconditions(StartRequest{Cwd: t.TempDir(), ExpectedGitHead: head}); err == nil {
		t.Fatal("nonrepo passed head precondition")
	}
	if err := checkGitPreconditions(StartRequest{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("omitted preconditions rejected nonrepo: %v", err)
	}
}

func TestExpectedGitHeadValidationAndLegacyIdentity(t *testing.T) {
	m := newManager(t, managerOpts{})
	for _, head := range []string{"short12", strings.Repeat("a", 39), strings.Repeat("g", 40), strings.Repeat("a", 41), "main"} {
		if _, err := m.normalizeRequest(StartRequest{Cwd: t.TempDir(), ExpectedGitHead: head}); err == nil {
			t.Errorf("invalid full ID accepted: %q", head)
		}
	}
	for _, n := range []int{40, 64} {
		req, err := m.normalizeRequest(StartRequest{Cwd: t.TempDir(), ExpectedGitHead: strings.Repeat("A", n)})
		if err != nil || req.ExpectedGitHead != strings.Repeat("a", n) {
			t.Fatalf("hex case/length normalization: %+v %v", req, err)
		}
	}
	req := StartRequest{Prompt: "review", ExpectedGitBranch: "main", ExpectedGitHead: strings.Repeat("a", 40)}
	args := buildAgyArgs(req)
	meta := jobstore.Meta{Args: args}
	if sameRequest(meta, req, args) {
		t.Fatal("legacy argv-only job matched newly added preconditions")
	}
	meta.ExpectedGitBranch, meta.ExpectedGitHead = req.ExpectedGitBranch, req.ExpectedGitHead
	if !sameRequest(meta, req, args) {
		t.Fatal("matching legacy metadata expectations refused")
	}
}

func TestWorkspaceResolutionExposesFullHeadForPreconditions(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	createTestGitRepo(t, repo)
	head, err := readGitHead(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	out := resolveWorkspaceDirectory(repo, "main")
	if !out.Resolved || out.HeadFull != head || !strings.HasPrefix(head, out.Head) || out.Head == head {
		t.Fatalf("full head missing or short display changed: %+v", out)
	}
	_, _, worktrees := inspectProjectGit(repo)
	if len(worktrees) != 1 || worktrees[0].HeadFull != head {
		t.Fatalf("discovery lost full head: %+v", worktrees)
	}
	resolved := resolveProjectWorktree(ProjectInfo{Name: "repo", Worktrees: worktrees}, "main")
	if !resolved.Resolved || resolved.HeadFull != head {
		t.Fatalf("project selection lost full head: %+v", resolved)
	}
	denied := resolveWorkspaceDirectory(repo, "wrong")
	if denied.HeadFull != "" {
		t.Fatalf("unresolved selection leaked full head: %+v", denied)
	}
}
