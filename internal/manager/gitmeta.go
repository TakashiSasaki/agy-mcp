package manager

import (
	"context"
	"strings"
	"time"
)

// DetectGitMetadata inspects the working directory to resolve current Git
// branch name, short commit SHA, and uncommitted modification state.
// If cwd is empty, not a Git repository, or git commands fail/timeout, it
// returns empty strings and nil dirty without failing the caller.
func DetectGitMetadata(cwd string) (branch, head string, dirty *bool) {
	if cwd == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// 1. Resolve the branch name, which is absent on detached HEAD.
	branch, _ = readGitBranch(ctx, cwd)

	// 2. Resolve short commit SHA (rev-parse)
	cmdHead := probeCmd(ctx, "git", "-C", cwd, "rev-parse", "--short", "HEAD")
	if out, err := cmdHead.Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	if head == "" {
		return "", "", nil
	}

	return branch, head, gitDirty(ctx, cwd)
}

func gitDirty(ctx context.Context, cwd string) *bool {
	// Ignore untracked files to avoid scanning large build directories.
	cmd := probeCmd(ctx, "git", "-C", cwd, "status", "--porcelain", "-uno")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return new(strings.TrimSpace(string(out)) != "")
}
