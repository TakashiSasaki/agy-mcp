package manager

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func validExpectedGitHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	_, err := hex.DecodeString(head)
	return err == nil
}

// checkGitPreconditions verifies caller expectations for new work. It does not
// lock the repository, so another process can still change it after this check.
func checkGitPreconditions(req StartRequest) error {
	if req.ExpectedGitBranch == "" && req.ExpectedGitHead == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if req.ExpectedGitBranch != "" {
		out, err := probeCmd(ctx, "git", "-C", req.Cwd, "symbolic-ref", "--short", "-q", "HEAD").Output()
		if err != nil {
			return refusedStart("workspace_mismatch", "change_request", false, fmt.Errorf("cannot verify expected Git branch %q in %q: %w", req.ExpectedGitBranch, req.Cwd, err))
		}
		if actual := strings.TrimSpace(string(out)); actual != req.ExpectedGitBranch {
			return refusedStart("workspace_mismatch", "change_request", false, fmt.Errorf("git branch in %q is %q, expected %q", req.Cwd, actual, req.ExpectedGitBranch))
		}
	}
	if req.ExpectedGitHead != "" {
		actual, err := readGitHead(ctx, req.Cwd)
		if err != nil {
			return refusedStart("workspace_mismatch", "change_request", false, fmt.Errorf("cannot verify expected Git HEAD %q in %q: %w", req.ExpectedGitHead, req.Cwd, err))
		}
		if actual != req.ExpectedGitHead {
			return refusedStart("workspace_mismatch", "change_request", false, fmt.Errorf("git HEAD in %q is %q, expected %q", req.Cwd, actual, req.ExpectedGitHead))
		}
	}
	return nil
}

// readGitHead returns an unabridged commit object ID without a model turn.
func readGitHead(ctx context.Context, cwd string) (string, error) {
	out, err := probeCmd(ctx, "git", "-C", cwd, "rev-parse", "--verify", "HEAD^{commit}").Output()
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(out))
	if !validExpectedGitHead(head) {
		return "", fmt.Errorf("git returned an invalid full commit ID")
	}
	return head, nil
}
