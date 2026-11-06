package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Mutation guard: readLogBytes must advance over invalid bytes while preserving
// valid runes, even when every byte around a page boundary is a continuation.
func TestLogsInvalidUTF8MakesProgress(t *testing.T) {
	for _, raw := range []string{strings.Repeat("\x80", 20), "A" + strings.Repeat("\x80", 20) + "Z", "あ😀éZ", "ABC\xff\x80😀\xe3"} {
		t.Run(hex.EncodeToString([]byte(raw)), func(t *testing.T) {
			m, _, id, dir := stageEvidenceJob(t, false)
			if err := os.WriteFile(jobstore.ErrPath(dir), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cursor := ""
			var end int64
			var displayed strings.Builder
			for range len(raw) + 1 {
				page, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", FromStart: cursor == "", Cursor: cursor, MaxBytes: 4})
				if err != nil {
					t.Fatal(err)
				}
				if page.StartOffset != end || page.EndOffset <= end || page.EndOffset-end > 4 || !utf8.ValidString(page.Content) {
					t.Fatalf("log did not advance within its byte budget: %+v", page)
				}
				chunk := raw[page.StartOffset:page.EndOffset]
				digest := sha256.Sum256([]byte(chunk))
				if page.SHA256 != hex.EncodeToString(digest[:]) || page.Content != strings.ToValidUTF8(chunk, "\uFFFD") {
					t.Fatalf("page no longer describes its original bytes: %+v", page)
				}
				displayed.WriteString(page.Content)
				end, cursor = page.EndOffset, page.NextCursor
				if !page.HasMore {
					break
				}
			}
			if end != int64(len(raw)) || (utf8.ValidString(raw) && displayed.String() != raw) {
				t.Fatalf("incomplete or damaged log: consumed %d/%d, text %q", end, len(raw), displayed.String())
			}
		})
	}
}

// Mutation guard: initial-tail alignment must skip only the remainder of a
// valid rune, not discard standalone invalid continuation bytes.
func TestLogsTailPreservesInvalidBytes(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"ABC\x80\x80\x80Z", "\uFFFDZ"},
		{"ABC😀Z", "Z"},
	} {
		m, _, id, dir := stageEvidenceJob(t, false)
		if err := os.WriteFile(jobstore.ErrPath(dir), []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		page, err := m.Logs(LogRequest{JobID: id, Stream: "stderr", TailBytes: 4, MaxBytes: 4})
		if err != nil || page.Content != tc.want || page.EndOffset != int64(len(tc.raw)) || page.HasMore {
			t.Fatalf("tail dropped invalid bytes or split a valid rune: %+v %v", page, err)
		}
	}
}
