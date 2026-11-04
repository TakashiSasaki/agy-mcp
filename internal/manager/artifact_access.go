package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func normalizeArtifactDirs(dirs []string) ([]string, error) {
	if len(dirs) > 16 {
		return nil, fmt.Errorf("artifact_dirs must contain at most 16 directories")
	}
	var out []string
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("artifact_dirs must contain absolute directory paths")
		}
		normalized, err := normalizeCwd(dir)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, normalized) {
			out = append(out, normalized)
		}
	}
	return out, nil
}

func artifactBase(meta jobstore.Meta, jobDir string) string {
	if len(meta.ArtifactDirs) > 0 {
		return meta.ArtifactDirs[0]
	}
	if meta.Cwd != "" {
		return meta.Cwd
	}
	return jobDir
}

func artifactScope(meta jobstore.Meta) string {
	if len(meta.ArtifactDirs) > 0 {
		return "declared"
	}
	return "legacy_workspace"
}

func artifactTarget(meta jobstore.Meta, jobDir, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if strings.HasPrefix(path, "job/") || strings.HasPrefix(path, ".job/") {
		return filepath.Join(jobDir, strings.TrimPrefix(strings.TrimPrefix(path, "job/"), ".job/")), nil
	}
	target := filepath.Join(artifactBase(meta, jobDir), path)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		candidate := filepath.Join(jobDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return target, nil
}

func (m *Manager) artifactExpiry(id string) string {
	if m.cfg.JobTTL <= 0 {
		return ""
	}
	if completed, ok := m.store.CompletedAt(id); ok {
		return completed.Add(m.cfg.JobTTL).UTC().Format(time.RFC3339Nano)
	}
	return ""
}

// The selected root is an authority boundary. Open relative to its live root
// handle so inner symlinks and renames cannot redirect a read outside it.
func openArtifactFile(roots []string, target string) (*os.File, error) {
	var cause error
	for _, base := range roots {
		rel, err := filepath.Rel(base, target)
		if err != nil || !isSubpathOf(base, target) {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			cause = err
			continue
		}
		f, err := root.OpenFile(rel, artifactReadFlags, 0)
		_ = root.Close()
		if err != nil {
			cause = err
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			if err == nil {
				err = fmt.Errorf("artifact must be a regular file")
			}
			cause = err
			continue
		}
		return f, nil
	}
	if cause != nil {
		return nil, fmt.Errorf("access denied or artifact unavailable: %w", cause)
	}
	return nil, fmt.Errorf("access denied: artifact is outside job-associated roots")
}

func openArtifactDirectory(roots []string, target string) (*os.Root, error) {
	var cause error
	for _, base := range roots {
		rel, err := filepath.Rel(base, target)
		if err != nil || !isSubpathOf(base, target) {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			cause = err
			continue
		}
		sub, err := root.OpenRoot(rel)
		_ = root.Close()
		if err == nil {
			return sub, nil
		}
		cause = err
	}
	if cause != nil {
		return nil, fmt.Errorf("access denied or artifact directory unavailable: %w", cause)
	}
	return nil, fmt.Errorf("access denied: directory is outside job-associated roots")
}
