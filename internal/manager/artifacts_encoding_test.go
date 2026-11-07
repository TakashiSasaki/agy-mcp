package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Normalize invalid bytes before bounding displayed UTF-8. Removing this
// normalization/bound permits JSON encoding to expand past max_bytes.
func TestArtifactInvalidUTF8ContentBudget(t *testing.T) {
	for _, raw := range []string{strings.Repeat("\xff", 4096), strings.Repeat("a\xff", 2048), "ab\xff日\n", "\xe6\x97"} {
		for _, limit := range []int{1, 2, 3, 4, 1024} {
			m, id, root := setupTestJobWithFiles(t)
			path := filepath.Join(root, "invalid.log")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			page, err := m.ReadArtifact(ReadArtifactRequest{JobID: id, Path: path, MaxBytes: limit})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			var wire ReadArtifactResult
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(page.Content) || len(wire.Content) > limit || wire.Content != page.Content {
				t.Fatalf("content exceeds UTF-8 budget: limit=%d raw=%d displayed=%d", limit, len(page.Content), len(wire.Content))
			}
			digest := sha256.Sum256([]byte(raw))
			if page.SHA256 != hex.EncodeToString(digest[:]) || page.SizeBytes != int64(len(raw)) {
				t.Fatal("original evidence metadata changed")
			}
		}
	}
}
