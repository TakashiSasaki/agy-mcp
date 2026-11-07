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
	Subpath    string `json:"subpath,omitempty" jsonschema:"optional subdirectory within job-associated roots; defaults to the first artifact_dirs entry, otherwise workspace or job directory"`
	Pattern    string `json:"pattern,omitempty" jsonschema:"optional glob pattern to filter filenames (e.g. *.json, *.log, *.xml). Matches filename only"`
	MaxEntries int    `json:"max_entries,omitempty" jsonschema:"maximum number of file entries to return (default 100, max 500)"`
}

type artifactEntryOutput struct {
	Path      string `json:"path" jsonschema:"path relative to the listed root directory"`
	SizeBytes int64  `json:"size_bytes" jsonschema:"file size in bytes"`
	SHA256    string `json:"sha256,omitempty" jsonschema:"hex-encoded SHA-256 digest when available within the listing hash budget"`
	ModTime   string `json:"mod_time" jsonschema:"RFC 3339 modification timestamp in UTC"`
	IsDir     bool   `json:"is_dir" jsonschema:"true if entry is a directory"`
}

type listArtifactsOutput struct {
	JobID        string                `json:"job_id" jsonschema:"job id whose artifacts were listed"`
	Root         string                `json:"root" jsonschema:"base root directory that was listed"`
	Entries      []artifactEntryOutput `json:"entries" jsonschema:"files and artifacts found matching criteria"`
	Scope        string                `json:"scope" jsonschema:"declared when artifact_dirs were supplied, otherwise legacy_workspace; this is read authorization, not proof that the job created every file"`
	Roots        []string              `json:"roots" jsonschema:"directories explicitly associated with this job's artifact reads"`
	ExpiresAt    string                `json:"expires_at,omitempty" jsonschema:"completion or durable first-observed-dead time plus job TTL, matching GC eligibility; absent while running, before interrupted death is observed, or when expiry is disabled; external file retention is independent"`
	LimitReached bool                  `json:"limit_reached" jsonschema:"the entry limit was reached; more matching files may exist"`
}

type readArtifactInput struct {
	JobID     string `json:"job_id" jsonschema:"handle of the job whose artifact to read (from agy_run or agy_run_sync)"`
	Path      string `json:"path" jsonschema:"file within artifact_dirs and job state directory; without artifact_dirs the legacy workspace/add-dir scope applies"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"1-based line number to start reading from (default 1); beyond EOF returns empty content without clamping"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"1-based line number to end reading at, inclusive (0 = read to end or max_bytes)"`
	TailLines int    `json:"tail_lines,omitempty" jsonschema:"if greater than 0, reads the trailing N lines of the file (useful for logs)"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum UTF-8 content bytes after replacing invalid encoding (default 64 KiB, max 256 KiB); original-file hash and size are unchanged"`
}

type readArtifactOutput struct {
	JobID      string `json:"job_id" jsonschema:"job id of the artifact"`
	Path       string `json:"path" jsonschema:"requested file path"`
	SizeBytes  int64  `json:"size_bytes" jsonschema:"total file size in bytes on disk"`
	SHA256     string `json:"sha256" jsonschema:"hex-encoded SHA-256 digest of the complete file on disk for evidence verification"`
	TotalLines int    `json:"total_lines" jsonschema:"total line count of the file"`
	StartLine  int    `json:"start_line" jsonschema:"normalized requested start line; may exceed end_line for an empty range"`
	EndLine    int    `json:"end_line" jsonschema:"selected end line capped at total_lines; zero for an empty file; content may be byte-truncated"`
	Truncated  bool   `json:"truncated" jsonschema:"true if content was truncated by max_bytes limit"`
	Content    string `json:"content" jsonschema:"text content of the requested line range or tail"`
}

func registerArtifacts(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyListArtifacts,
		Title:       "List job artifacts and logs",
		Annotations: annReadLocal,
		Description: "Discover evidence files associated with an agy job without another model turn. When artifact_dirs were provided at creation, reads use those explicit directories and the job state directory; otherwise legacy workspace/add-dir roots apply. scope and roots make this distinction explicit. Association authorizes reading and does not prove the job created a file. Inner symlink escapes are rejected. Common dependency/cache directories are skipped. SHA-256 is optional. Listing hashes files up to 32 MiB each, within a shared 128 MiB and two-second hashing budget. Use agy_read_artifact for the full digest of an individual stable file. expires_at is job-handle GC eligibility, not a guarantee about external file retention. Pair with agy_read_artifact for stable-file line ranges, agy_logs for live tails and byte cursors, and agy_verification for JUnit, native Go test JSON or restricted flat TAP reports. agy_status reports job lifecycle only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listArtifactsInput) (*mcp.CallToolResult, listArtifactsOutput, error) {
		res, err := mgr.ListArtifactsContext(ctx, manager.ListArtifactsRequest{
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
			Scope:   res.Scope, Roots: res.Roots, ExpiresAt: res.ExpiresAt, LimitReached: res.LimitReached,
		}, nil
	})

	addTool(s, &mcp.Tool{
		Name:        toolAgyReadArtifact,
		Title:       "Read job artifact or log file",
		Annotations: annReadLocal,
		Description: "Read stable evidence files with line ranges and byte limits without a model turn. artifact_dirs restrict reads to explicit job-associated directories plus job state; older/default jobs retain the legacy workspace/add-dir scope. Path traversal and inner symlink escapes are rejected using directory handles. Hashing and content use the same opened file; a detected file change fails the read instead of presenting inconsistent evidence. The full file is scanned for SHA-256 and line counts using fixed memory, then the selected range is read with bounded buffering. Long lines have no fixed size limit. CRLF is normalized and an unterminated final line gets a newline. A start_line beyond EOF returns empty content with the requested start_line and end_line equal to total_lines. Use agy_logs with stream artifact for bounded live byte reads, agy_verification to parse a selected JUnit, native Go test JSON or restricted flat TAP report, agy_list_artifacts for discovery, or agy_status for lifecycle state.",
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
