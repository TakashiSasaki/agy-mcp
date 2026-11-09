package manager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectCloudSyncStatusAndFilters(t *testing.T) {
	projDir := t.TempDir()
	dataDir := t.TempDir()

	// 1. Cloud-synced project (UUID ID, isWorkspaceOnly: false)
	cloudJSON := `{
		"id": "11111111-2222-3333-4444-555555555555",
		"name": "Cloud Project",
		"projectResources": {"resources": [{"folderUri": "file:///data/cloud-repo"}]},
		"isWorkspaceOnly": false
	}`
	if err := os.WriteFile(filepath.Join(projDir, "cloud.json"), []byte(cloudJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Local-only project (UUID ID, isWorkspaceOnly: true)
	localJSON := `{
		"id": "99999999-8888-7777-6666-555555555555",
		"name": "Local Only Project",
		"projectResources": {"resources": [{"folderUri": "file:///data/local-repo"}]},
		"isWorkspaceOnly": true
	}`
	if err := os.WriteFile(filepath.Join(projDir, "local.json"), []byte(localJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Default CLI project
	cliJSON := `{
		"id": "default-cli-project",
		"name": "CLI Workspace",
		"projectResources": {"resources": [{"folderUri": "file:///data/cli-repo"}]}
	}`
	if err := os.WriteFile(filepath.Join(projDir, "default-cli-project.json"), []byte(cliJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Unregistered repository in dataDir
	unregisteredRepo := filepath.Join(dataDir, "unregistered-repo")
	if err := os.MkdirAll(filepath.Join(unregisteredRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		projectsDir: projDir,
		dataDir:     dataDir,
	}

	// Test 1: List all projects (no filter)
	allProjs, err := mgr.ListProjects("")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(allProjs) != 4 {
		t.Fatalf("len(allProjs) = %d, want 4", len(allProjs))
	}

	byID := make(map[string]ProjectInfo)
	for _, p := range allProjs {
		byID[p.ID] = p
	}

	// Verify cloud project
	cloud := byID["11111111-2222-3333-4444-555555555555"]
	if !cloud.IsCloudSynced || cloud.SyncScope != "cloud" {
		t.Errorf("cloud project status mismatch: %+v", cloud)
	}

	// Verify local-only project
	local := byID["99999999-8888-7777-6666-555555555555"]
	if local.IsCloudSynced || local.SyncScope != "local_only" {
		t.Errorf("local project status mismatch: %+v", local)
	}

	// Verify CLI default project
	cli := byID["default-cli-project"]
	if cli.IsCloudSynced || cli.SyncScope != "cli_default" {
		t.Errorf("cli project status mismatch: %+v", cli)
	}

	// Verify unregistered repo
	unreg := byID["unregistered-repo"]
	if unreg.IsCloudSynced || unreg.SyncScope != "unregistered" {
		t.Errorf("unregistered repo status mismatch: %+v", unreg)
	}

	// Test 2: Filter by cloud_only
	cloudOnly, err := mgr.ListProjects("", "cloud_only")
	if err != nil {
		t.Fatalf("ListProjects cloud_only: %v", err)
	}
	if len(cloudOnly) != 1 || cloudOnly[0].ID != cloud.ID {
		t.Errorf("expected only cloud project, got: %+v", cloudOnly)
	}

	// Test 3: Filter by local_only
	localOnly, err := mgr.ListProjects("", "local_only")
	if err != nil {
		t.Fatalf("ListProjects local_only: %v", err)
	}
	if len(localOnly) != 3 {
		t.Errorf("expected 3 non-cloud projects, got: %d", len(localOnly))
	}
}
