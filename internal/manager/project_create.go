package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// ProjectCreation is a registration read back from the host's AGY profile.
type ProjectCreation struct {
	ProjectID string `json:"project_id" jsonschema:"registered project ID for the project argument of agy_run or agy_run_sync"`
	Name      string `json:"name" jsonschema:"display name stored by AGY; new registrations take their name from the folder"`
	Root      string `json:"root" jsonschema:"canonical path of the requested server-side folder"`
	Created   bool   `json:"created" jsonschema:"true when this call created the registration; false when it returned an existing project for the folder"`
}

// CreateProject registers a folder using AGY itself. One eligible registration
// for the exact folder makes a repeat call safe after a lost response.
func (m *Manager) CreateProject(ctx context.Context, root string) (ProjectCreation, error) {
	if runtime.GOOS != cloudLinux {
		return ProjectCreation{}, fmt.Errorf("project creation requires the qualified Linux AGY deployment")
	}
	if !filepath.IsAbs(root) {
		return ProjectCreation{}, fmt.Errorf("project root must be an absolute server-side directory")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ProjectCreation{}, fmt.Errorf("resolve project root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return ProjectCreation{}, fmt.Errorf("inspect project root: %w", err)
	}
	if !rootInfo.IsDir() {
		return ProjectCreation{}, fmt.Errorf("project root must be a directory")
	}
	// MEASURED against AGY 1.5.3: --new-project stores file:// followed by
	// the raw path. Reject paths that the reader would misinterpret before
	// AGY writes an unusable registration.
	if resource, ok := projectResourcePath("file://" + root); !ok || resource != root {
		return ProjectCreation{}, fmt.Errorf("AGY %s cannot safely register this folder path", cloudAGYVersion)
	}
	if !filepath.IsAbs(m.projectsDir) || !filepath.IsAbs(m.conversationsDBPath) {
		return ProjectCreation{}, fmt.Errorf("cannot resolve the AGY profile")
	}
	if err := jobstore.MkdirAllDurable(m.cloudDir(), 0o700); err != nil {
		return ProjectCreation{}, err
	}
	release, locked, err := m.cloudManualGate()
	if err != nil {
		return ProjectCreation{}, err
	}
	defer release()
	if !locked {
		return ProjectCreation{}, fmt.Errorf("cannot lock the AGY profile for project creation")
	}

	project, err := m.projectForFolder(rootInfo)
	if err != nil {
		return ProjectCreation{}, err
	}
	if project != nil {
		return ProjectCreation{ProjectID: project.ID, Name: project.Name, Root: root}, nil
	}
	if err := m.registerProjectFolder(ctx, root); err != nil {
		return ProjectCreation{}, err
	}
	project, err = m.projectForFolder(rootInfo)
	if err != nil {
		return ProjectCreation{}, err
	}
	if project == nil {
		return ProjectCreation{}, fmt.Errorf("AGY did not register the requested folder; inspect agy_list_projects before retrying")
	}
	return ProjectCreation{ProjectID: project.ID, Name: project.Name, Root: root, Created: true}, nil
}

func (m *Manager) projectForFolder(root os.FileInfo) (*ProjectInfo, error) {
	projects, _, err := discoverRegisteredProjects(m.projectsDir)
	if err != nil {
		return nil, fmt.Errorf("cannot verify the project registry: %w", err)
	}
	var found *ProjectInfo
	for _, project := range projects {
		info, err := os.Stat(project.Root)
		if err != nil || !os.SameFile(root, info) {
			continue
		}
		if found != nil && (project.ID != found.ID || project.Name != found.Name || project.IsCloudSynced != found.IsCloudSynced) {
			return nil, fmt.Errorf("folder has multiple project registrations; select a project ID with agy_list_projects")
		}
		found = project
	}
	if found != nil && !found.IsCloudSynced {
		return nil, fmt.Errorf("folder already has a project that cannot be used for cloud job sync")
	}
	return found, nil
}

func (m *Manager) registerProjectFolder(ctx context.Context, root string) error {
	agy, err := m.cfg.AgyBinary()
	if err != nil {
		return err
	}
	agy, err = filepath.EvalSymlinks(agy)
	if err != nil {
		return err
	}
	if err := checkProjectCLIPin(agy); err != nil {
		return err
	}
	if err := m.checkCloudAGYVersion(ctx, agy); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// MEASURED against AGY 1.5.3: startup writes the registration before
	// opening the terminal UI. A detached process with empty stdin exits
	// without a turn. The caller checks the registration, not CLI messages.
	cmd := probeCmd(ctx, agy, "--new-project", "--remote-control=false", "--remote-agent=false")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), agyDisableAutoUpdate)
	proc.ConfigureSession(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("AGY project registration interrupted: %w; retry with the same folder to recover an existing registration", err)
	}
	return nil
}

// AGY 1.5.3 was observed replacing its executable during isolated launches
// despite the documented updater setting. Check the actual executable path.
func checkProjectCLIPin(agy string) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("project creation requires an unprivileged AGY service")
	}
	for _, path := range []string{agy, filepath.Dir(agy)} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("project creation requires a write-protected AGY executable and containing directory")
		}
	}
	return nil
}
