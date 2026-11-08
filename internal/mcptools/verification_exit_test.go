package mcptools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Removing ExitCodePath forwarding must fail the wire verdict assertion.
func TestVerificationCommandExitWire(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	store := jobstore.New(state)
	const id = "command-receipt"
	if _, err := store.Create(jobstore.Meta{ID: id, ArtifactDirs: []string{root}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"tests.tap": "1..1\nok 1 - child\n", "make.exit": "2\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := manager.New(config.Config{StateDir: state, JobTTL: time.Hour})
	cs := connect(t, mgr, nil)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: map[string]any{"job_id": id, "path": "tests.tap", "format": "tap", "exit_code_path": "make.exit", "max_failures": 0}})
	if err != nil || res.IsError {
		t.Fatalf("verification: %+v %v", res, err)
	}
	out := structuredMap(t, res)
	if out["verification_outcome"] != "failed" || out["report_outcome"] != "passed" || out["passed"] != float64(1) || out["command_exit"].(map[string]any)["exit_code"] != float64(2) {
		t.Fatalf("command failure hidden: %+v", out)
	}
}

func TestVerificationCommandReceiptWire(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	store := jobstore.New(state)
	const id, report = "paired-receipt", "1..1\nok 1 - child\n"
	if _, err := store.Create(jobstore.Meta{ID: id, ArtifactDirs: []string{root}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(t, "tests.tap", report)
	write(t, "rerun.tap", report) // Same bytes from a different invocation.
	write(t, "make.exit", "0\n")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(report)))
	receipt := func(path, code string) string {
		return fmt.Sprintf(`{"report_path":%q,"report_sha256":%q,"exit_code":%s}`, path, digest, code)
	}
	cs := connect(t, manager.New(config.Config{StateDir: state, JobTTL: time.Hour}), nil)
	for _, tc := range []struct {
		name, contents, outcome string
		both                    bool
	}{
		{name: "matching failed Make", contents: receipt("tests.tap", "2"), outcome: "failed"},
		{name: "matching success", contents: receipt("tests.tap", "0"), outcome: "passed"},
		{name: "wrong invocation with byte-identical report", contents: receipt("rerun.tap", "0")},
		{name: "stale receipt after report replacement", contents: receipt("tests.tap", "0")},
		{name: "ambiguous receipt", contents: strings.TrimSuffix(receipt("tests.tap", "2"), "}") + `,"exit_code":0}`},
		{name: "missing exit", contents: fmt.Sprintf(`{"report_path":"tests.tap","report_sha256":%q}`, digest)},
		{name: "null exit", contents: receipt("tests.tap", "null")},
		{name: "out of range exit", contents: receipt("tests.tap", "2147483648")},
		{name: "oversized", contents: receipt("tests.tap", "0") + strings.Repeat(" ", 8192)},
		{name: "conflicting receipt inputs", contents: receipt("tests.tap", "0"), both: true},
		{name: "explicit empty receipt path"},
		{name: "explicit null receipt path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, "receipt.json", tc.contents)
			args := map[string]any{"job_id": id, "path": "tests.tap", "format": "tap", "command_receipt_path": "receipt.json", "max_failures": 0}
			switch tc.name {
			case "stale receipt after report replacement":
				write(t, "tests.tap", "1..1\nok 1 - replacement\n")
				t.Cleanup(func() { write(t, "tests.tap", report) })
			case "explicit empty receipt path":
				args["command_receipt_path"] = ""
			case "explicit null receipt path":
				args["command_receipt_path"] = nil
			}
			if tc.both {
				args["exit_code_path"] = "make.exit"
			}
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAgyVerification, Arguments: args})
			if err != nil || res.IsError != (tc.outcome == "") {
				t.Fatalf("verification: %+v %v, want outcome %q", res, err, tc.outcome)
			}
			if tc.outcome == "" {
				return
			}
			out := structuredMap(t, res)
			exit, ok := out["command_exit"].(map[string]any)
			wantReceiptDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.contents)))
			if !ok || out["verification_outcome"] != tc.outcome || out["report_outcome"] != "passed" || out["sha256"] != digest ||
				exit["source"] != "command_receipt_artifact" || exit["sha256"] != wantReceiptDigest {
				t.Fatalf("verification receipt lost: %+v", out)
			}
		})
	}
}
