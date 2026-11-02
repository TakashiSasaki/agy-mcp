package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDetectGitMetadataInRealRepo(t *testing.T) {
	t.Parallel()
	// Test on the current repository worktree
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	branch, head, dirty := DetectGitMetadata(cwd)
	if head == "" {
		t.Fatalf("expected non-empty git head in repo cwd: %s", cwd)
	}
	if branch == "" {
		t.Logf("running on detached HEAD at commit: %s", head)
	} else {
		t.Logf("detected branch: %s, head: %s, dirty: %v", branch, head, dirty != nil && *dirty)
	}
}

func TestDetectGitMetadataNonGitDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	branch, head, dirty := DetectGitMetadata(dir)
	if branch != "" || head != "" || dirty != nil {
		t.Fatalf("expected empty git metadata in non-git dir, got branch=%q head=%q dirty=%v", branch, head, dirty)
	}
}

func TestDetectGitMetadataFreshRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmdInit := exec.Command("git", "init", "-b", "feature-test", dir)
	if err := cmdInit.Run(); err != nil {
		t.Skipf("git init not available: %v", err)
	}
	cmdConfig1 := exec.Command("git", "-C", dir, "config", "user.name", "Test")
	_ = cmdConfig1.Run()
	cmdConfig2 := exec.Command("git", "-C", dir, "config", "user.email", "test@example.com")
	_ = cmdConfig2.Run()

	testFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdAdd := exec.Command("git", "-C", dir, "add", "hello.txt")
	if err := cmdAdd.Run(); err != nil {
		t.Fatal(err)
	}
	cmdCommit := exec.Command("git", "-C", dir, "commit", "-m", "initial commit")
	if err := cmdCommit.Run(); err != nil {
		t.Fatal(err)
	}

	branch, head, dirty := DetectGitMetadata(dir)
	if branch != "feature-test" {
		t.Errorf("branch = %q, want feature-test", branch)
	}
	if head == "" {
		t.Error("expected non-empty head SHA")
	}
	if dirty == nil || *dirty {
		t.Errorf("expected clean repository, got dirty=%v", dirty)
	}

	// Modify file
	if err := os.WriteFile(testFile, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, dirtyAfter := DetectGitMetadata(dir)
	if dirtyAfter == nil || !*dirtyAfter {
		t.Errorf("expected dirty repository after file edit, got %v", dirtyAfter)
	}
}
