package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func setupTestJobWithFiles(t *testing.T) (mgr *Manager, jobID, workspace string) {
	t.Helper()
	workspace = t.TempDir()
	stateDir := t.TempDir()

	mgr = New(config.Config{StateDir: stateDir})

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

	jobID = "test-job-artifact-01"
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

func TestListArtifactHashChecksDiscoveryIdentity(t *testing.T) {
	stableTime := time.Unix(1770000000, 0)
	for _, tc := range []struct {
		name        string
		replacement string
		newInode    bool
		mtime       time.Time
	}{
		{name: "stable", replacement: "old", mtime: stableTime},
		{name: "different inode with same metadata", replacement: "new", newInode: true, mtime: stableTime},
		{name: "changed size on same inode", replacement: "larger", mtime: stableTime},
		{name: "changed mtime on same inode", replacement: "new", mtime: stableTime.Add(time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "artifact")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, stableTime, stableTime); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			discovered, err := root.Lstat("artifact")
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "stable" {
				updatedPath := path
				if tc.newInode {
					updatedPath = filepath.Join(dir, "replacement")
				}
				if err := os.WriteFile(updatedPath, []byte(tc.replacement), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(updatedPath, tc.mtime, tc.mtime); err != nil {
					t.Fatal(err)
				}
				if tc.newInode {
					if err := os.Rename(updatedPath, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			digest, read := listArtifactHash(t.Context(), f, discovered, 1024)
			if tc.name == "stable" {
				want := sha256.Sum256([]byte("old"))
				if digest != hex.EncodeToString(want[:]) || read != 3 {
					t.Fatalf("unchanged digest/read = %q/%d", digest, read)
				}
			} else if digest != "" || read != 0 {
				t.Fatalf("stale discovery metadata acquired a hash or spent budget: %q/%d", digest, read)
			}
		})
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

func TestListArtifactsBoundsTotalHashBytes(t *testing.T) {
	m, id, root := setupTestJobWithFiles(t)
	for i := range 5 {
		path := filepath.Join(root, fmt.Sprintf("data-%d.bin", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 32<<20); err != nil {
			t.Fatal(err)
		}
	}
	res, err := m.ListArtifacts(ListArtifactsRequest{JobID: id, Pattern: "*.bin"})
	if err != nil || len(res.Entries) != 5 {
		t.Fatalf("listing omitted metadata: %+v, %v", res, err)
	}
	var hashed int64
	for _, entry := range res.Entries {
		if entry.SHA256 != "" {
			hashed += entry.SizeBytes
		}
	}
	if hashed <= 0 || hashed > 128<<20 {
		t.Fatalf("hashed bytes = %d, want positive and at most 128 MiB", hashed)
	}
}

func TestListArtifactsHonorsCancellation(t *testing.T) {
	m, id, _ := setupTestJobWithFiles(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.ListArtifactsContext(ctx, ListArtifactsRequest{JobID: id}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing = %v", err)
	}
}
