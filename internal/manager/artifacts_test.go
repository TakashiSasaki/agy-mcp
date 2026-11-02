package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func setupTestJobWithFiles(t *testing.T) (*Manager, string, string) {
	t.Helper()
	workspace := t.TempDir()
	stateDir := t.TempDir()

	mgr := New(config.Config{StateDir: stateDir})

	// Create test artifact files in workspace
	artDir := filepath.Join(workspace, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	f1 := filepath.Join(artDir, "receipt.json")
	if err := os.WriteFile(f1, []byte("{\"status\":\"ok\",\"code\":0}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f2 := filepath.Join(artDir, "build.log")
	logContent := "line 1: start\nline 2: compile\nline 3: test\nline 4: done\n"
	if err := os.WriteFile(f2, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create ignored noise dir
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobID := "test-job-artifact-01"
	dir, err := mgr.store.Create(jobstore.Meta{
		ID:  jobID,
		Cwd: workspace,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("assistant response\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return mgr, jobID, workspace
}

func TestListArtifactsHappyPath(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	res, err := mgr.ListArtifacts(ListArtifactsRequest{
		JobID:   jobID,
		Subpath: "artifacts",
	})
	if err != nil {
		t.Fatalf("ListArtifacts failed: %v", err)
	}

	if len(res.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 (receipt.json and build.log)", len(res.Entries))
	}

	foundReceipt := false
	for _, e := range res.Entries {
		if e.Path == "receipt.json" {
			foundReceipt = true
			expected := sha256.Sum256([]byte("{\"status\":\"ok\",\"code\":0}\n"))
			expectedHex := hex.EncodeToString(expected[:])
			if e.SHA256 != expectedHex {
				t.Errorf("receipt SHA256 = %s, want %s", e.SHA256, expectedHex)
			}
		}
	}
	if !foundReceipt {
		t.Error("receipt.json not found in entries")
	}
}

func TestListArtifactsSkipsNoiseDirectories(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	res, err := mgr.ListArtifacts(ListArtifactsRequest{
		JobID: jobID,
	})
	if err != nil {
		t.Fatalf("ListArtifacts failed: %v", err)
	}

	for _, e := range res.Entries {
		if strings.Contains(e.Path, ".git") {
			t.Errorf("noise entry found in list: %s", e.Path)
		}
	}
}

func TestListArtifactsRejectsPathTraversal(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	_, err := mgr.ListArtifacts(ListArtifactsRequest{
		JobID:   jobID,
		Subpath: "../../../../../etc",
	})
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected access denied error, got: %v", err)
	}
}

func TestReadArtifactHappyPath(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	res, err := mgr.ReadArtifact(ReadArtifactRequest{
		JobID: jobID,
		Path:  "artifacts/build.log",
	})
	if err != nil {
		t.Fatalf("ReadArtifact failed: %v", err)
	}

	if res.TotalLines != 4 {
		t.Errorf("total_lines = %d, want 4", res.TotalLines)
	}
	if !strings.Contains(res.Content, "line 1: start") {
		t.Errorf("missing content in: %q", res.Content)
	}
}

func TestReadArtifactLineRangeAndTail(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	// Test line range 2 to 3
	r1, err := mgr.ReadArtifact(ReadArtifactRequest{
		JobID:     jobID,
		Path:      "artifacts/build.log",
		StartLine: 2,
		EndLine:   3,
	})
	if err != nil {
		t.Fatalf("ReadArtifact lines 2-3: %v", err)
	}
	if r1.Content != "line 2: compile\nline 3: test\n" {
		t.Errorf("got %q, want lines 2-3", r1.Content)
	}

	// Test tail lines 2
	r2, err := mgr.ReadArtifact(ReadArtifactRequest{
		JobID:     jobID,
		Path:      "artifacts/build.log",
		TailLines: 2,
	})
	if err != nil {
		t.Fatalf("ReadArtifact tail 2: %v", err)
	}
	if r2.Content != "line 3: test\nline 4: done\n" {
		t.Errorf("got %q, want last 2 lines", r2.Content)
	}
}

func TestReadArtifactRejectsOutOfBounds(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	_, err := mgr.ReadArtifact(ReadArtifactRequest{
		JobID: jobID,
		Path:  "../../../../../../etc/passwd",
	})
	if err == nil {
		t.Fatal("expected access denied error for /etc/passwd")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected access denied, got %v", err)
	}
}

func TestReadArtifactCanReadJobDirFiles(t *testing.T) {
	mgr, jobID, _ := setupTestJobWithFiles(t)

	res, err := mgr.ReadArtifact(ReadArtifactRequest{
		JobID: jobID,
		Path:  "out",
	})
	if err != nil {
		t.Fatalf("ReadArtifact out file failed: %v", err)
	}

	if !strings.Contains(res.Content, "assistant response") {
		t.Errorf("unexpected content for out: %q", res.Content)
	}
}
