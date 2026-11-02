package manager

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Default limits for artifact tools.
const (
	DefaultListArtifactsLimit = 100
	MaxListArtifactsLimit     = 500
	DefaultReadArtifactBytes  = 64 * 1024  // 64 KiB
	MaxReadArtifactBytes      = 256 * 1024 // 256 KiB
	maxHashFileSize           = 32 << 20   // 32 MiB
)

var defaultIgnoredDirs = map[string]bool{
	".git":         true,
	".venv":        true,
	"venv":         true,
	"node_modules": true,
	"uv_cache":     true,
	"python_home":  true,
	"__pycache__":  true,
	".tox":         true,
	".cache":       true,
	".build":       true,
}

// ArtifactEntry represents a file or directory discovered within an allowed job root.
type ArtifactEntry struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
	ModTime   string `json:"mod_time"`
	IsDir     bool   `json:"is_dir"`
}

// ListArtifactsRequest specifies parameters for listing artifacts of a job.
type ListArtifactsRequest struct {
	JobID      string
	Subpath    string
	Pattern    string
	MaxEntries int
}

// ListArtifactsResult contains the listed entries.
type ListArtifactsResult struct {
	JobID   string          `json:"job_id"`
	Root    string          `json:"root"`
	Entries []ArtifactEntry `json:"entries"`
}

// ReadArtifactRequest specifies parameters for reading an artifact file.
type ReadArtifactRequest struct {
	JobID     string
	Path      string
	StartLine int
	EndLine   int
	TailLines int
	MaxBytes  int
}

// ReadArtifactResult contains the file content and metadata.
type ReadArtifactResult struct {
	JobID      string `json:"job_id"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	TotalLines int    `json:"total_lines"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Truncated  bool   `json:"truncated"`
	Content    string `json:"content"`
}

func isSubpathOf(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func resolveAllowedRoots(meta jobstore.Meta, jobDir string) []string {
	var roots []string
	addRoot := func(p string) {
		if p == "" {
			return
		}
		clean := filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			roots = append(roots, filepath.Clean(resolved))
		}
		roots = append(roots, clean)
	}

	addRoot(meta.Cwd)
	for i := 0; i < len(meta.Args); i++ {
		arg := meta.Args[i]
		var d string
		if arg == "--add-dir" && i+1 < len(meta.Args) {
			d = meta.Args[i+1]
			i++
		} else if strings.HasPrefix(arg, "--add-dir=") {
			d = strings.TrimPrefix(arg, "--add-dir=")
		}
		addRoot(d)
	}
	addRoot(jobDir)
	return roots
}

func (m *Manager) resolveJobAndRoots(jobID string) (jobstore.Meta, string, []string, error) {
	meta, err := m.store.Load(jobID)
	if err != nil {
		return jobstore.Meta{}, "", nil, fmt.Errorf("load job %s: %w", jobID, err)
	}
	jobDir, err := m.store.Dir(jobID)
	if err != nil {
		return jobstore.Meta{}, "", nil, fmt.Errorf("job dir %s: %w", jobID, err)
	}
	roots := resolveAllowedRoots(meta, jobDir)
	return meta, jobDir, roots, nil
}

func fileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ListArtifacts lists artifacts within allowed job directories.
func (m *Manager) ListArtifacts(req ListArtifactsRequest) (ListArtifactsResult, error) {
	meta, jobDir, roots, err := m.resolveJobAndRoots(req.JobID)
	if err != nil {
		return ListArtifactsResult{}, err
	}

	limit := req.MaxEntries
	if limit <= 0 {
		limit = DefaultListArtifactsLimit
	} else if limit > MaxListArtifactsLimit {
		limit = MaxListArtifactsLimit
	}

	var baseDir string
	if req.Subpath == "" {
		if meta.Cwd != "" {
			baseDir = meta.Cwd
		} else {
			baseDir = jobDir
		}
	} else if filepath.IsAbs(req.Subpath) {
		baseDir = filepath.Clean(req.Subpath)
	} else if strings.HasPrefix(req.Subpath, "job/") || strings.HasPrefix(req.Subpath, ".job/") {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(req.Subpath, "job/"), ".job/")
		baseDir = filepath.Join(jobDir, trimmed)
	} else if meta.Cwd != "" {
		baseDir = filepath.Join(meta.Cwd, req.Subpath)
	} else {
		baseDir = filepath.Join(jobDir, req.Subpath)
	}

	cleanBase := filepath.Clean(baseDir)
	resolvedBase := cleanBase
	if r, err := filepath.EvalSymlinks(cleanBase); err == nil {
		resolvedBase = filepath.Clean(r)
	}

	confined := false
	for _, root := range roots {
		if isSubpathOf(root, resolvedBase) || isSubpathOf(root, cleanBase) {
			confined = true
			break
		}
	}
	if !confined {
		return ListArtifactsResult{}, fmt.Errorf("access denied: path %q is outside allowed job directories", req.Subpath)
	}

	info, err := os.Stat(resolvedBase)
	if err != nil {
		return ListArtifactsResult{}, fmt.Errorf("stat %s: %w", req.Subpath, err)
	}
	if !info.IsDir() {
		return ListArtifactsResult{}, fmt.Errorf("path %s is not a directory", req.Subpath)
	}

	var entries []ArtifactEntry
	walkErr := filepath.WalkDir(resolvedBase, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if defaultIgnoredDirs[d.Name()] && p != resolvedBase {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if req.Pattern != "" {
			matched, mErr := filepath.Match(req.Pattern, d.Name())
			if mErr != nil || !matched {
				return nil
			}
		}

		rel, err := filepath.Rel(resolvedBase, p)
		if err != nil {
			rel = filepath.Base(p)
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		size := fi.Size()
		modTime := fi.ModTime().UTC().Format(time.RFC3339)

		var hash string
		if size <= maxHashFileSize {
			if h, err := fileSHA256(p); err == nil {
				hash = h
			}
		}

		entries = append(entries, ArtifactEntry{
			Path:      rel,
			SizeBytes: size,
			SHA256:    hash,
			ModTime:   modTime,
			IsDir:     false,
		})

		if len(entries) >= limit {
			return fs.SkipAll
		}
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return ListArtifactsResult{}, fmt.Errorf("walk directory: %w", walkErr)
	}

	return ListArtifactsResult{
		JobID:   req.JobID,
		Root:    cleanBase,
		Entries: entries,
	}, nil
}

// ReadArtifact reads content from an artifact file within allowed job directories.
func (m *Manager) ReadArtifact(req ReadArtifactRequest) (ReadArtifactResult, error) {
	meta, jobDir, roots, err := m.resolveJobAndRoots(req.JobID)
	if err != nil {
		return ReadArtifactResult{}, err
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultReadArtifactBytes
	} else if maxBytes > MaxReadArtifactBytes {
		maxBytes = MaxReadArtifactBytes
	}

	p := req.Path
	if p == "" {
		return ReadArtifactResult{}, errors.New("path must not be empty")
	}

	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else if strings.HasPrefix(p, "job/") || strings.HasPrefix(p, ".job/") {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(p, "job/"), ".job/")
		target = filepath.Join(jobDir, trimmed)
	} else {
		// First check meta.Cwd
		if meta.Cwd != "" {
			target = filepath.Join(meta.Cwd, p)
		} else {
			target = filepath.Join(jobDir, p)
		}
		// If it does not exist under cwd, check whether it is a jobDir file (e.g. out, err, result.json)
		if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
			jobCandidate := filepath.Join(jobDir, p)
			if _, jerr := os.Stat(jobCandidate); jerr == nil {
				target = jobCandidate
			}
		}
	}

	cleanTarget := filepath.Clean(target)
	resolvedTarget := cleanTarget
	if r, err := filepath.EvalSymlinks(cleanTarget); err == nil {
		resolvedTarget = filepath.Clean(r)
	}

	confined := false
	for _, root := range roots {
		if isSubpathOf(root, resolvedTarget) || isSubpathOf(root, cleanTarget) {
			confined = true
			break
		}
	}
	if !confined {
		return ReadArtifactResult{}, fmt.Errorf("access denied: path %q is outside allowed job directories", p)
	}

	info, err := os.Stat(resolvedTarget)
	if err != nil {
		return ReadArtifactResult{}, fmt.Errorf("stat %s: %w", p, err)
	}
	if info.IsDir() {
		return ReadArtifactResult{}, fmt.Errorf("path %s is a directory, not a regular file", p)
	}

	fullHash, err := fileSHA256(resolvedTarget)
	if err != nil {
		return ReadArtifactResult{}, fmt.Errorf("hash file: %w", err)
	}

	f, err := os.Open(resolvedTarget)
	if err != nil {
		return ReadArtifactResult{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	// Buffer up to 1MB per line
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanErr := scanner.Err(); scanErr != nil && !errors.Is(scanErr, io.EOF) {
		return ReadArtifactResult{}, fmt.Errorf("scan lines: %w", scanErr)
	}

	totalLines := len(lines)
	startLine := req.StartLine
	endLine := req.EndLine

	if req.TailLines > 0 {
		startLine = totalLines - req.TailLines + 1
		if startLine < 1 {
			startLine = 1
		}
		endLine = totalLines
	} else {
		if startLine < 1 {
			startLine = 1
		}
		if endLine <= 0 || endLine > totalLines {
			endLine = totalLines
		}
		if startLine > totalLines && totalLines > 0 {
			startLine = totalLines
		}
	}

	var selected []string
	if totalLines > 0 && startLine <= endLine && startLine <= totalLines {
		selected = lines[startLine-1 : endLine]
	}

	content := strings.Join(selected, "\n")
	if len(selected) > 0 && totalLines > 0 {
		content += "\n"
	}

	truncated := false
	if len(content) > maxBytes {
		truncated = true
		content = truncateUTF8(content, maxBytes)
	}

	return ReadArtifactResult{
		JobID:      req.JobID,
		Path:       p,
		SizeBytes:  info.Size(),
		SHA256:     fullHash,
		TotalLines: totalLines,
		StartLine:  startLine,
		EndLine:    endLine,
		Truncated:  truncated,
		Content:    content,
	}, nil
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
