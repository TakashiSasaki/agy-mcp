package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CommandExitEvidence is caller-selected report evidence, not a supervisor
// receipt. Its association with the selected test report is caller-supplied.
type CommandExitEvidence struct {
	Source   string `json:"source" jsonschema:"exit_code_artifact; reported command status, not independently witnessed execution"`
	Path     string `json:"path" jsonschema:"caller-selected exit-code artifact within this job's associated roots"`
	SHA256   string `json:"sha256" jsonschema:"SHA-256 of the exact exit-code artifact bytes"`
	ExitCode int32  `json:"exit_code" jsonschema:"reported signed 32-bit decimal command status; zero success, any nonzero failure"`
}

// VerificationWithExitCode preserves report-only behavior unless a caller
// explicitly selects a command receipt. A successful child report must not
// hide the enclosing command's reported failure. Incomplete reports stay unknown.
func (m *Manager) VerificationWithExitCode(jobID, path, format, exitPath string) (VerificationResult, error) {
	out, err := m.VerificationReport(jobID, path, format)
	if err != nil || exitPath == "" {
		return out, err
	}
	exit, err := m.readCommandExit(jobID, exitPath)
	if err != nil {
		return VerificationResult{}, err
	}
	out.ReportOutcome, out.CommandExit = out.VerificationOutcome, &exit
	if exit.ExitCode != 0 && out.VerificationOutcome == "passed" {
		out.VerificationOutcome = StateFailed
	}
	return out, nil
}

func (m *Manager) readCommandExit(jobID, path string) (CommandExitEvidence, error) {
	meta, dir, roots, err := m.resolveJobAndRoots(jobID)
	if err != nil {
		return CommandExitEvidence{}, err
	}
	target, err := artifactTarget(meta, dir, path)
	if err != nil {
		return CommandExitEvidence{}, err
	}
	f, err := openArtifactFile(roots, target)
	if err != nil {
		return CommandExitEvidence{}, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return CommandExitEvidence{}, err
	}
	const maxExitBytes = 64
	data, err := io.ReadAll(io.LimitReader(f, maxExitBytes+1))
	if err != nil {
		return CommandExitEvidence{}, err
	}
	if len(data) > maxExitBytes {
		return CommandExitEvidence{}, fmt.Errorf("exit-code artifact exceeds 64 bytes")
	}
	after, err := f.Stat()
	if err != nil {
		return CommandExitEvidence{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return CommandExitEvidence{}, fmt.Errorf("exit-code artifact changed while reading; retry after the producer closes it")
	}
	code, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return CommandExitEvidence{}, fmt.Errorf("exit-code artifact must contain one signed 32-bit decimal integer")
	}
	digest := sha256.Sum256(data)
	return CommandExitEvidence{Source: "exit_code_artifact", Path: path, SHA256: hex.EncodeToString(digest[:]), ExitCode: int32(code)}, nil
}
