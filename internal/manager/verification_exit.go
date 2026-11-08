package manager

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CommandExitEvidence is caller-selected report evidence, not a supervisor
// receipt. Its association with the selected test report is caller-supplied.
type CommandExitEvidence struct {
	Source   string `json:"source" jsonschema:"exit_code_artifact or command_receipt_artifact; reported command status"`
	Path     string `json:"path" jsonschema:"caller-selected exit-code or command receipt artifact within this job's associated roots"`
	SHA256   string `json:"sha256" jsonschema:"SHA-256 of the exact exit-code or command receipt artifact bytes"`
	ExitCode int32  `json:"exit_code" jsonschema:"reported signed 32-bit decimal command status; zero success, any nonzero failure"`
}

// VerificationWithExitCode accepts a decimal exit artifact or an explicitly
// bound JSON receipt. Incomplete reports stay unknown with either input.
func (m *Manager) VerificationWithExitCode(jobID, path, format, exitPath string) (VerificationResult, error) {
	if receiptPath, ok := strings.CutPrefix(exitPath, "receipt:"); ok {
		return m.verificationWithCommandReceipt(jobID, path, format, receiptPath)
	}
	out, err := m.VerificationReport(jobID, path, format)
	if err != nil || exitPath == "" {
		return out, err
	}
	exit, err := m.readCommandExit(jobID, exitPath)
	if err != nil {
		return VerificationResult{}, err
	}
	return applyCommandExit(out, exit), nil
}

// verificationWithCommandReceipt checks a producer's report path and digest
// before using the exit status bundled with them in the receipt.
func (m *Manager) verificationWithCommandReceipt(jobID, path, format, receiptPath string) (VerificationResult, error) {
	const maxReceiptBytes = 8 << 10
	data, err := m.readCommandEvidence(jobID, receiptPath, "command receipt", maxReceiptBytes)
	if err != nil {
		return VerificationResult{}, err
	}
	var receipt struct {
		ReportPath   string `json:"report_path"`
		ReportSHA256 string `json:"report_sha256"`
		ExitCode     *int32 `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return VerificationResult{}, fmt.Errorf("invalid command receipt: %w", err)
	}
	if receipt.ReportPath == "" || receipt.ExitCode == nil || len(receipt.ReportSHA256) != sha256.Size*2 {
		return VerificationResult{}, fmt.Errorf("command receipt requires report_path, a 64-character hexadecimal report_sha256 and a signed 32-bit exit_code")
	}
	if _, err := hex.DecodeString(receipt.ReportSHA256); err != nil {
		return VerificationResult{}, fmt.Errorf("command receipt report_sha256 must be hexadecimal")
	}
	if receipt.ReportPath != path {
		return VerificationResult{}, fmt.Errorf("command receipt report_path does not match the selected path")
	}
	out, err := m.VerificationReport(jobID, path, format)
	if err != nil {
		return VerificationResult{}, err
	}
	if !strings.EqualFold(receipt.ReportSHA256, out.SHA256) {
		return VerificationResult{}, fmt.Errorf("command receipt report_sha256 does not match the selected report bytes")
	}
	return applyCommandExit(out, commandExitEvidence(receiptPath, "command_receipt_artifact", data, *receipt.ExitCode)), nil
}

func applyCommandExit(out VerificationResult, exit CommandExitEvidence) VerificationResult {
	out.ReportOutcome, out.CommandExit = out.VerificationOutcome, &exit
	if exit.ExitCode != 0 && out.VerificationOutcome == "passed" {
		out.VerificationOutcome = StateFailed
	}
	return out
}

func (m *Manager) readCommandExit(jobID, path string) (CommandExitEvidence, error) {
	const maxExitBytes = 64
	data, err := m.readCommandEvidence(jobID, path, "exit-code artifact", maxExitBytes)
	if err != nil {
		return CommandExitEvidence{}, err
	}
	code, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return CommandExitEvidence{}, fmt.Errorf("exit-code artifact must contain one signed 32-bit decimal integer")
	}
	return commandExitEvidence(path, "exit_code_artifact", data, int32(code)), nil
}

func commandExitEvidence(path, source string, data []byte, code int32) CommandExitEvidence {
	digest := sha256.Sum256(data)
	return CommandExitEvidence{Source: source, Path: path, SHA256: hex.EncodeToString(digest[:]), ExitCode: code}
}

func (m *Manager) readCommandEvidence(jobID, path, label string, limit int64) ([]byte, error) {
	meta, dir, roots, err := m.resolveJobAndRoots(jobID)
	if err != nil {
		return nil, err
	}
	target, err := artifactTarget(meta, dir, path)
	if err != nil {
		return nil, err
	}
	f, err := openArtifactFile(roots, target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("%s changed while reading; retry after the producer closes it", label)
	}
	return data, nil
}
