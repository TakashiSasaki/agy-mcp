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

	// 1. Resolve branch name (symbolic-ref); on detached HEAD or shallow clones it fails silently
	cmdBranch := probeCmd(ctx, "git", "-C", cwd, "symbolic-ref", "--short", "-q", "HEAD")
	if out, err := cmdBranch.Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}

	// 2. Resolve short commit SHA (rev-parse)
	cmdHead := probeCmd(ctx, "git", "-C", cwd, "rev-parse", "--short", "HEAD")
	if out, err := cmdHead.Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	if head == "" {
		return "", "", nil
	}

	// 3. Fast dirty check (-uno ignores untracked files to avoid scanning huge untracked build dirs)
	cmdStatus := probeCmd(ctx, "git", "-C", cwd, "status", "--porcelain", "-uno")
	if out, err := cmdStatus.Output(); err == nil {
		isDirty := len(strings.TrimSpace(string(out))) > 0
		dirty = &isDirty
	}

	return branch, head, dirty
}
