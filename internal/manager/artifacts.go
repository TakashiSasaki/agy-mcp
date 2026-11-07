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
	JobID        string          `json:"job_id"`
	Root         string          `json:"root"`
	Entries      []ArtifactEntry `json:"entries"`
	Scope        string          `json:"scope"`
	Roots        []string        `json:"roots"`
	ExpiresAt    string          `json:"expires_at,omitempty"`
	LimitReached bool            `json:"limit_reached"`
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
	seen := make(map[string]bool)
	appendRoot := func(path string) {
		if !seen[path] {
			seen[path] = true
			roots = append(roots, path)
		}
	}
	addRoot := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(meta.Cwd, p)
		}
		clean := filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			appendRoot(filepath.Clean(resolved))
		}
		appendRoot(clean)
	}

	if len(meta.ArtifactDirs) > 0 {
		for _, dir := range meta.ArtifactDirs {
			addRoot(dir)
		}
	} else {
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

func fileSHA256(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
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
		baseDir = artifactBase(meta, jobDir)
	} else if filepath.IsAbs(req.Subpath) {
		baseDir = filepath.Clean(req.Subpath)
	} else if strings.HasPrefix(req.Subpath, "job/") || strings.HasPrefix(req.Subpath, ".job/") {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(req.Subpath, "job/"), ".job/")
		baseDir = filepath.Join(jobDir, trimmed)
	} else {
		baseDir = filepath.Join(artifactBase(meta, jobDir), req.Subpath)
	}

	cleanBase := filepath.Clean(baseDir)
	baseRoot, err := openArtifactDirectory(roots, cleanBase)
	if err != nil {
		return ListArtifactsResult{}, err
	}
	defer func() { _ = baseRoot.Close() }()
	entries := make([]ArtifactEntry, 0)
	limitReached := false
	walkErr := fs.WalkDir(baseRoot.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if defaultIgnoredDirs[d.Name()] && p != "." {
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

		rel, err := filepath.Rel(".", filepath.FromSlash(p))
		if err != nil {
			rel = filepath.Base(p)
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		size := fi.Size()
		modTime := fi.ModTime().UTC().Format(time.RFC3339)

		var hash string
		if size <= maxHashFileSize {
			f, openErr := baseRoot.OpenFile(filepath.FromSlash(p), artifactReadFlags, 0)
			if openErr == nil {
				opened, statErr := f.Stat()
				if statErr == nil && opened.Mode().IsRegular() {
					if h, hashErr := fileSHA256(f); hashErr == nil {
						hash = h
					}
				}
				_ = f.Close()
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
			limitReached = true
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
		Scope:   artifactScope(meta), Roots: roots, ExpiresAt: m.artifactExpiry(meta), LimitReached: limitReached,
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

	target, err := artifactTarget(meta, jobDir, req.Path)
	if err != nil {
		return ReadArtifactResult{}, err
	}
	f, err := openArtifactFile(roots, target)
	if err != nil {
		return ReadArtifactResult{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ReadArtifactResult{}, err
	}
	fullHash, err := fileSHA256(f)
	if err != nil {
		return ReadArtifactResult{}, fmt.Errorf("hash artifact: %w", err)
	}
	p := req.Path
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

	after, statErr := f.Stat()
	if statErr != nil {
		return ReadArtifactResult{}, statErr
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return ReadArtifactResult{}, fmt.Errorf("artifact changed during read; retry or use agy_logs for a live file")
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
