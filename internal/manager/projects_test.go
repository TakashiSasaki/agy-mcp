package manager

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func createTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo dir: %v", err)
	}
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "user.email", "test@example.com"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run git %v: %v (output: %s)", args, err, string(out))
		}
	}
	// Initial commit
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = dir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (output: %s)", err, string(out))
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = dir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (output: %s)", err, string(out))
	}
}

func TestListProjectsAndResolveWorkspace(t *testing.T) {
	tempBase := filepath.Join(os.TempDir(), "agy-test-projects")
	if err := os.MkdirAll(tempBase, 0o755); err != nil {
		t.Fatalf("mkdir tempBase: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempBase) })

	testProjectsDir := filepath.Join(tempBase, "projects-config")
	testDataDir := filepath.Join(tempBase, "data")
	if err := os.MkdirAll(testProjectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects-config: %v", err)
	}
	if err := os.MkdirAll(testDataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}

	// 1. Create a git repo under testDataDir: test-repo
	repoDir := filepath.Join(testDataDir, "test-repo")
	createTestGitRepo(t, repoDir)

	// Add remote
	cmdRemote := exec.Command("git", "remote", "add", "github", "https://github.com/example/test-repo.git")
	cmdRemote.Dir = repoDir
	if out, err := cmdRemote.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (output: %s)", err, string(out))
	}

	// 2. Create another repo outside testDataDir, registered via projects json
	externalRepoDir := filepath.Join(tempBase, "external-app")
	createTestGitRepo(t, externalRepoDir)
	cmdRemoteExt := exec.Command("git", "remote", "add", "origin", "https://github.com/example/external-app.git")
	cmdRemoteExt.Dir = externalRepoDir
	if out, err := cmdRemoteExt.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (output: %s)", err, string(out))
	}

	projConfig := map[string]any{
		"id":   "project-ext",
		"name": "External App",
		"projectResources": map[string]any{
			"resources": []map[string]any{
				{
					"folderUri": "file://" + externalRepoDir,
					"gitFolder": map[string]any{
						"folderUri":     "file://" + externalRepoDir,
						"defaultBranch": "main",
					},
				},
			},
		},
	}
	configBytes, err := json.Marshal(projConfig)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testProjectsDir, "ext.json"), configBytes, 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	m := &Manager{
		projectsDir: testProjectsDir,
		dataDir:     testDataDir,
	}

	// Test ListProjects with no query
	projs, err := m.ListProjects("")
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}
	if len(projs) != 2 {
		t.Fatalf("got %d projects, want 2. Projects: %+v", len(projs), projs)
	}

	// Verify project fields
	var extProj, testRepoProj *ProjectInfo
	for i := range projs {
		if projs[i].Name == "External App" {
			extProj = &projs[i]
		} else if projs[i].Name == "test-repo" {
			testRepoProj = &projs[i]
		}
	}
	if extProj == nil {
		t.Fatalf("External App project not found in list")
	}
	if extProj.RepositoryURL != "https://github.com/example/external-app.git" {
		t.Errorf("External App repo URL = %q, want https://github.com/example/external-app.git", extProj.RepositoryURL)
	}
	if len(extProj.Worktrees) != 1 {
		t.Errorf("External App worktrees count = %d, want 1", len(extProj.Worktrees))
	}

	if testRepoProj == nil {
		t.Fatalf("test-repo project not found in list")
	}
	if testRepoProj.RepositoryURL != "https://github.com/example/test-repo.git" {
		t.Errorf("test-repo repo URL = %q, want https://github.com/example/test-repo.git", testRepoProj.RepositoryURL)
	}

	// Test ListProjects query filter
	filtered, err := m.ListProjects("external")
	if err != nil {
		t.Fatalf("ListProjects filter failed: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Name != "External App" {
		t.Errorf("ListProjects filter = %+v, want 1 project [External App]", filtered)
	}

	// Test ResolveWorkspace with direct path
	resDirect, err := m.ResolveWorkspace(repoDir, "")
	if err != nil {
		t.Fatalf("ResolveWorkspace direct path failed: %v", err)
	}
	if !resDirect.Resolved || resDirect.Path != repoDir {
		t.Errorf("resDirect = %+v, want resolved with path %q", resDirect, repoDir)
	}

	// Test ResolveWorkspace with project name
	resName, err := m.ResolveWorkspace("External App", "")
	if err != nil {
		t.Fatalf("ResolveWorkspace by name failed: %v", err)
	}
	if !resName.Resolved || resName.Path != externalRepoDir {
		t.Errorf("resName = %+v, want resolved with path %q", resName, externalRepoDir)
	}

	// Test ResolveWorkspace with project ID
	resID, err := m.ResolveWorkspace("project-ext", "")
	if err != nil {
		t.Fatalf("ResolveWorkspace by ID failed: %v", err)
	}
	if !resID.Resolved || resID.Path != externalRepoDir {
		t.Errorf("resID = %+v, want resolved with path %q", resID, externalRepoDir)
	}

	// Test ResolveWorkspace with nonexistent project
	resNone, err := m.ResolveWorkspace("does-not-exist", "")
	if err != nil {
		t.Fatalf("ResolveWorkspace nonexistent unexpected error: %v", err)
	}
	if resNone.Resolved {
		t.Errorf("resNone should not be resolved: %+v", resNone)
	}

	// Test ResolveWorkspace with empty query
	resEmpty, err := m.ResolveWorkspace("", "")
	if err != nil {
		t.Fatalf("ResolveWorkspace empty query unexpected error: %v", err)
	}
	if resEmpty.Resolved || resEmpty.Error == "" {
		t.Errorf("resEmpty should fail with error message: %+v", resEmpty)
	}
}

func TestParseGitRemotes(t *testing.T) {
	sample := "github\thttps://github.com/example/repo.git (fetch)\n" +
		"github\thttps://github.com/example/repo.git (push)\n" +
		"upstream\thttps://github.com/upstream/repo.git (fetch)\n" +
		"upstream\thttps://github.com/upstream/repo.git (push)\n"

	remotes := parseGitRemotes(sample)
	if remotes["github"] != "https://github.com/example/repo.git" {
		t.Errorf("github remote = %q, want https://github.com/example/repo.git", remotes["github"])
	}
	if remotes["upstream"] != "https://github.com/upstream/repo.git" {
		t.Errorf("upstream remote = %q, want https://github.com/upstream/repo.git", remotes["upstream"])
	}
}

func TestResolveWorkspaceMultipleWorktrees(t *testing.T) {
	tempBase := filepath.Join(os.TempDir(), "agy-test-multi-wt")
	if err := os.MkdirAll(tempBase, 0o755); err != nil {
		t.Fatalf("mkdir tempBase: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempBase) })

	testProjectsDir := filepath.Join(tempBase, "projects")
	testDataDir := filepath.Join(tempBase, "data")
	_ = os.MkdirAll(testProjectsDir, 0o755)
	_ = os.MkdirAll(testDataDir, 0o755)

	mainWt := filepath.Join(tempBase, "main-wt")
	featureWt := filepath.Join(tempBase, "feature-wt")
	createTestGitRepo(t, mainWt)
	createTestGitRepo(t, featureWt)

	// Create project config pointing to multiple worktrees
	projConfig := map[string]any{
		"id":   "multi-proj",
		"name": "Multi Project",
		"projectResources": map[string]any{
			"resources": []map[string]any{
				{
					"folderUri": "file://" + mainWt,
					"gitFolder": map[string]any{
						"folderUri":     "file://" + mainWt,
						"defaultBranch": "main",
					},
				},
			},
		},
	}
	b, err := json.Marshal(projConfig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = os.WriteFile(filepath.Join(testProjectsDir, "multi.json"), b, 0o644)

	m := &Manager{
		projectsDir: testProjectsDir,
		dataDir:     testDataDir,
	}

	res, err := m.ResolveWorkspace("multi-proj", "")
	if err != nil {
		t.Fatalf("ResolveWorkspace: %v", err)
	}
	if !res.Resolved || res.Path != mainWt {
		t.Errorf("res = %+v, want resolved to %s", res, mainWt)
	}
}
