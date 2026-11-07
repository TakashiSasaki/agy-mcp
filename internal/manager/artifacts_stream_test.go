package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Mutation guards: restoring EOF clamping fails beyond EOF; dropping pendingCR
// handling fails split CRLF; omitting UTF-8 truncation fails unicode boundary.
// Restoring the Scanner implementation fails the long-line cases.
func TestReadArtifactStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, input, want         string
		start, end, tail, limit   int
		lines, wantStart, wantEnd int
		truncated                 bool
	}{
		{name: "long earlier line tail", input: strings.Repeat("x", 2<<20) + "\nfinal failure\n", want: "final failure\n", tail: 1, limit: 128, lines: 2, wantStart: 2, wantEnd: 2},
		{name: "selected long line", input: strings.Repeat("x", 2<<20) + "\n", want: strings.Repeat("x", 32), limit: 32, lines: 1, wantStart: 1, wantEnd: 1, truncated: true},
		{name: "unicode boundary", input: strings.Repeat("a", 31) + "界\n", want: strings.Repeat("a", 31), limit: 32, lines: 1, wantStart: 1, wantEnd: 1, truncated: true},
		{name: "long unicode", input: strings.Repeat("界", 1<<20) + "\n", want: "界", limit: 4, lines: 1, wantStart: 1, wantEnd: 1, truncated: true},
		{name: "split CRLF", input: strings.Repeat("x", 64*1024-1) + "\r\nnext\r\n", want: strings.Repeat("x", 64*1024-1) + "\nnext\n", limit: 100000, lines: 2, wantStart: 1, wantEnd: 2},
		{name: "split interior CR", input: strings.Repeat("x", 64*1024-1) + "\rdata\n", want: strings.Repeat("x", 64*1024-1) + "\rdata\n", limit: 100000, lines: 1, wantStart: 1, wantEnd: 1},
		{name: "chunk ends CR at EOF", input: strings.Repeat("x", 64*1024-1) + "\r", want: strings.Repeat("x", 64*1024-1) + "\n", limit: 100000, lines: 1, wantStart: 1, wantEnd: 1},
		{name: "exact chunk at EOF", input: strings.Repeat("x", 64*1024), want: strings.Repeat("x", 64*1024) + "\n", limit: 100000, lines: 1, wantStart: 1, wantEnd: 1},
		{name: "empty", input: "", want: "", limit: 32, wantStart: 1},
		{name: "unterminated", input: "first\nlast", want: "last\n", tail: 1, limit: 32, lines: 2, wantStart: 2, wantEnd: 2},
		{name: "beyond EOF", input: "one\ntwo\nthree\n", want: "", start: 99, limit: 32, lines: 3, wantStart: 99, wantEnd: 3},
		{name: "blank lines", input: "\r\n\n", want: "\n\n", limit: 32, lines: 2, wantStart: 1, wantEnd: 2},
		{name: "huge tail request", input: "one\ntwo\n", want: "one", tail: int(^uint(0) >> 1), limit: 3, lines: 2, wantStart: 1, wantEnd: 2, truncated: true},
		{name: "range", input: "one\ntwo\nthree\n", want: "two\n", start: 2, end: 2, limit: 32, lines: 3, wantStart: 2, wantEnd: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, jobID, workspace := setupTestJobWithFiles(t)
			path := filepath.Join(workspace, "stream.log")
			if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := mgr.ReadArtifact(ReadArtifactRequest{JobID: jobID, Path: path, StartLine: tc.start, EndLine: tc.end, TailLines: tc.tail, MaxBytes: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			if out.Content != tc.want || out.Truncated != tc.truncated || out.TotalLines != tc.lines || out.StartLine != tc.wantStart || out.EndLine != tc.wantEnd {
				t.Fatalf("range mismatch: bytes=%d want=%d truncated=%v lines=%d range=%d..%d", len(out.Content), len(tc.want), out.Truncated, out.TotalLines, out.StartLine, out.EndLine)
			}
			digest := sha256.Sum256([]byte(tc.input))
			if out.SHA256 != hex.EncodeToString(digest[:]) || out.SizeBytes != int64(len(tc.input)) || !utf8.ValidString(out.Content) {
				t.Fatal("hash, size, or UTF-8 mismatch")
			}
		})
	}
}

// Mutation guard: removing the full-prefix early return rereads the entire
// selected log and fails the observed file-position bound.
func TestReadArtifactStopsAfterPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	data := strings.Repeat("x", 2<<20) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	content, truncated, err := readArtifactLines(f, 1, 1, 128)
	if err != nil || content != strings.Repeat("x", 128) || !truncated {
		t.Fatalf("bounded prefix: bytes=%d truncated=%v err=%v", len(content), truncated, err)
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil || offset >= int64(len(data)/2) {
		t.Fatalf("bounded selection reread the full large line: offset=%d err=%v", offset, err)
	}
}
