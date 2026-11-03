package mcptools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	toolAgyListArtifacts = "agy_list_artifacts"
	toolAgyReadArtifact  = "agy_read_artifact"
)

type listArtifactsInput struct {
	JobID      string `json:"job_id" jsonschema:"handle of the job whose artifacts to list (from agy_run or agy_run_sync)"`
	Subpath    string `json:"subpath,omitempty" jsonschema:"optional subdirectory to list within the job's allowed roots (e.g. artifacts, artifacts/ci-profile). Defaults to workspace root"`
	Pattern    string `json:"pattern,omitempty" jsonschema:"optional glob pattern to filter filenames (e.g. *.json, *.log, *.xml). Matches filename only"`
	MaxEntries int    `json:"max_entries,omitempty" jsonschema:"maximum number of file entries to return (default 100, max 500)"`
}

type artifactEntryOutput struct {
	Path      string `json:"path" jsonschema:"path relative to the listed root directory"`
	SizeBytes int64  `json:"size_bytes" jsonschema:"file size in bytes"`
	SHA256    string `json:"sha256,omitempty" jsonschema:"hex-encoded SHA-256 digest of file contents"`
	ModTime   string `json:"mod_time" jsonschema:"RFC 3339 modification timestamp in UTC"`
	IsDir     bool   `json:"is_dir" jsonschema:"true if entry is a directory"`
}

type listArtifactsOutput struct {
	JobID   string                `json:"job_id" jsonschema:"job id whose artifacts were listed"`
	Root    string                `json:"root" jsonschema:"base root directory that was listed"`
	Entries []artifactEntryOutput `json:"entries" jsonschema:"files and artifacts found matching criteria"`
}

type readArtifactInput struct {
	JobID     string `json:"job_id" jsonschema:"handle of the job whose artifact to read (from agy_run or agy_run_sync)"`
	Path      string `json:"path" jsonschema:"path to the file within allowed job directories (workspace, --add-dir, or job dir)"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"1-based line number to start reading from (default 1)"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"1-based line number to end reading at, inclusive (0 = read to end or max_bytes)"`
	TailLines int    `json:"tail_lines,omitempty" jsonschema:"if greater than 0, reads the trailing N lines of the file (useful for logs)"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum content bytes to return (default 64 KiB, max 256 KiB)"`
}

type readArtifactOutput struct {
	JobID      string `json:"job_id" jsonschema:"job id of the artifact"`
	Path       string `json:"path" jsonschema:"requested file path"`
	SizeBytes  int64  `json:"size_bytes" jsonschema:"total file size in bytes on disk"`
	SHA256     string `json:"sha256" jsonschema:"hex-encoded SHA-256 digest of the complete file on disk for evidence verification"`
	TotalLines int    `json:"total_lines" jsonschema:"total line count of the file"`
	StartLine  int    `json:"start_line" jsonschema:"1-based start line of the returned slice"`
	EndLine    int    `json:"end_line" jsonschema:"1-based end line of the returned slice"`
	Truncated  bool   `json:"truncated" jsonschema:"true if content was truncated by max_bytes limit"`
	Content    string `json:"content" jsonschema:"text content of the requested line range or tail"`
}

func registerArtifacts(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyListArtifacts,
		Title:       "List job artifacts and logs",
		Annotations: annReadLocal,
		Description: "List artifact, receipt, and report files produced by an agy job within its allowed directories. Use this to inspect evidence files (test receipts, JSON reports, profiles, logs) directly without spawning another model turn. Confined strictly to the job's workspace (cwd), extra dirs (--add-dir), and the job's state directory. Automatically ignores common dependency and cache directories (.git, .venv, node_modules, cache, __pycache__). Returns relative paths, file size, SHA-256 digest, and modification times. Pair with agy_read_artifact to retrieve contents, or check job status with agy_status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listArtifactsInput) (*mcp.CallToolResult, listArtifactsOutput, error) {
		res, err := mgr.ListArtifacts(manager.ListArtifactsRequest{
			JobID:      in.JobID,
			Subpath:    in.Subpath,
			Pattern:    in.Pattern,
			MaxEntries: in.MaxEntries,
		})
		if err != nil {
			return nil, listArtifactsOutput{}, err
		}
		entries := make([]artifactEntryOutput, 0, len(res.Entries))
		for _, e := range res.Entries {
			entries = append(entries, artifactEntryOutput{
				Path:      e.Path,
				SizeBytes: e.SizeBytes,
				SHA256:    e.SHA256,
				ModTime:   e.ModTime,
				IsDir:     e.IsDir,
			})
		}
		return nil, listArtifactsOutput{
			JobID:   res.JobID,
			Root:    res.Root,
			Entries: entries,
		}, nil
	})

	addTool(s, &mcp.Tool{
		Name:        toolAgyReadArtifact,
		Title:       "Read job artifact or log file",
		Annotations: annReadLocal,
		Description: "Read contents of a specific artifact, receipt, or log file produced by an agy job, with optional line range and byte limits. Use this to retrieve verification evidence without running another model turn. Strictly confined to the job's allowed directories (cwd, add-dirs, job dir); path traversal and out-of-bounds symlinks are rejected. Returns file size, full-file SHA-256 for verification, total lines, requested line range, and content. Use agy_list_artifacts to discover available files, or check job status with agy_status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readArtifactInput) (*mcp.CallToolResult, readArtifactOutput, error) {
		res, err := mgr.ReadArtifact(manager.ReadArtifactRequest{
			JobID:     in.JobID,
			Path:      in.Path,
			StartLine: in.StartLine,
			EndLine:   in.EndLine,
			TailLines: in.TailLines,
			MaxBytes:  in.MaxBytes,
		})
		if err != nil {
			return nil, readArtifactOutput{}, err
		}
		return nil, readArtifactOutput{
			JobID:      res.JobID,
			Path:       res.Path,
			SizeBytes:  res.SizeBytes,
			SHA256:     res.SHA256,
			TotalLines: res.TotalLines,
			StartLine:  res.StartLine,
			EndLine:    res.EndLine,
			Truncated:  res.Truncated,
			Content:    res.Content,
		}, nil
	})
}
