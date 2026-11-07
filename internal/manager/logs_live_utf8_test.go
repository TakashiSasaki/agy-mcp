package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Removing the running-stream incomplete-rune guard consumes bytes that are
// needed by a later page and changes a real split-write Unicode character.
func TestLogsLiveUTF8SplitWrites(t *testing.T) {
	for _, text := range []string{"日", "😀", "é"} {
		for split := 1; split < len(text); split++ {
			t.Run(text+strings.Repeat("x", split), func(t *testing.T) {
				m, store, id, dir := stageEvidenceJob(t, true)
				path := jobstore.ErrPath(dir)
				if err := os.WriteFile(path, []byte("abc"+text[:split]), 0o600); err != nil {
					t.Fatal(err)
				}
				first, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", FromStart: true, MaxBytes: 64})
				if err != nil {
					t.Fatal(err)
				}
				if first.Content != "abc" || first.EndOffset != 3 || !first.PendingUTF8 || first.HasMore {
					t.Fatalf("live partial rune consumed: content=%q end=%d", first.Content, first.EndOffset)
				}
				again, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", Cursor: first.NextCursor, MaxBytes: 64})
				if err != nil || again.Content != "" || again.EndOffset != 3 || !again.PendingUTF8 || again.HasMore {
					t.Fatalf("pending cursor moved: %+v %v", again, err)
				}
				if err := os.WriteFile(path, []byte("abc"+text+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				last, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", Cursor: first.NextCursor, MaxBytes: 64})
				if err != nil || last.Content != text+"\n" || last.EndOffset != int64(4+len(text)) || last.PendingUTF8 {
					t.Fatalf("split character not recovered: %+v %v", last, err)
				}
				digest := sha256.Sum256([]byte(text + "\n"))
				if last.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatal("hash no longer describes delivered original bytes")
				}
				if err := store.WriteExitCode(id, 0); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLogsPendingUTF8FlushesOnCompletion(t *testing.T) {
	m, store, id, dir := stageEvidenceJob(t, true)
	if err := os.WriteFile(jobstore.ErrPath(dir), []byte("\xffA\xe6\x97"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", FromStart: true, MaxBytes: 64})
	if err != nil || first.Content != "\uFFFDA" || first.EndOffset != 2 {
		t.Fatalf("invalid bytes or partial tail mishandled: %+v %v", first, err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
	last, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", Cursor: first.NextCursor, MaxBytes: 64})
	if err != nil || last.Content != "\uFFFD" || last.EndOffset != 4 || last.HasMore || last.PendingUTF8 {
		t.Fatalf("terminal invalid suffix stalled: %+v %v", last, err)
	}
}

// A first tail read can begin inside the currently incomplete rune. Retain its
// leading byte so a later append can still complete that character.
func TestLogsLiveUTF8TailStartsInsidePendingRune(t *testing.T) {
	m, store, id, dir := stageEvidenceJob(t, true)
	path := jobstore.ErrPath(dir)
	if err := os.WriteFile(path, []byte("abc日"[:5]), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", TailBytes: 1, MaxBytes: 4})
	if err != nil || first.Content != "" || first.StartOffset != 3 || first.EndOffset != 3 {
		t.Fatalf("tail lost partial leading byte: %+v %v", first, err)
	}
	if err := os.WriteFile(path, []byte("abc日\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	last, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", Cursor: first.NextCursor, MaxBytes: 4})
	if err != nil || last.Content != "日\n" || last.EndOffset != 7 {
		t.Fatalf("pending tail rune lost: %+v %v", last, err)
	}
	if err := store.WriteExitCode(id, 0); err != nil {
		t.Fatal(err)
	}
}
