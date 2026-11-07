package manager

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// Removing appendRoot's seen guard must make the equality assertions fail.
func TestResolveAllowedRootsUniqueOrder(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	evidence := filepath.Join(base, "evidence")
	jobDir := filepath.Join(base, "job")
	for _, dir := range []string{workspace, evidence, jobDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("declared_scope", func(t *testing.T) {
		meta := jobstore.Meta{Cwd: workspace, ArtifactDirs: []string{evidence, evidence}, Args: []string{"--add-dir", workspace}}
		got := resolveAllowedRoots(meta, jobDir)
		if want := []string{evidence, jobDir}; !slices.Equal(got, want) {
			t.Fatalf("roots = %v, want %v; declared scope must exclude execution roots", got, want)
		}
	})
	t.Run("legacy_order", func(t *testing.T) {
		meta := jobstore.Meta{Cwd: workspace, Args: []string{"--add-dir", workspace, "--add-dir=../evidence", "--add-dir", evidence}}
		got := resolveAllowedRoots(meta, jobDir)
		if want := []string{workspace, evidence, jobDir}; !slices.Equal(got, want) {
			t.Fatalf("roots = %v, want %v", got, want)
		}
	})
	t.Run("symlink_alias", func(t *testing.T) {
		alias := filepath.Join(base, "alias")
		if err := os.Symlink(evidence, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		meta := jobstore.Meta{Cwd: workspace, ArtifactDirs: []string{alias, evidence, alias}}
		got := resolveAllowedRoots(meta, jobDir)
		if want := []string{evidence, alias, jobDir}; !slices.Equal(got, want) {
			t.Fatalf("roots = %v, want %v; distinct canonical and lexical paths must remain", got, want)
		}
	})
}
