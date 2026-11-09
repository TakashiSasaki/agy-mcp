package manager

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Git can print the long form of a Windows temporary path whose environment
// uses an 8.3 alias. Construct expectations from the canonical checkout root.
func canonicalProjectTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func projectFileURI(path, host string) string {
	urlPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	return (&url.URL{Scheme: "file", Host: host, Path: urlPath}).String()
}

func writeProjectResources(t *testing.T, dir, file, id, name string, resources ...string) {
	t.Helper()
	items := make([]map[string]string, 0, len(resources))
	for _, resource := range resources {
		items = append(items, map[string]string{"folderUri": resource})
	}
	data, err := json.Marshal(map[string]any{"id": id, "name": name, "projectResources": map[string]any{"resources": items}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Mutation guards: decoding URL.Path and retaining a record per identity/root
// are necessary for encoded directories, multiple resources and shared roots.
func TestProjectResourceDiscoveryPreservesRootsAndIDs(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first space # %20 日本語"), filepath.Join(root, "second")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := t.TempDir()
	uri := projectFileURI(first, "")
	writeProjectResources(t, config, "a.json", "multi", "Same name", uri, second)
	writeProjectResources(t, config, "b.json", "alias", "Same name", uri)
	m := &Manager{projectsDir: config, dataDir: t.TempDir()}
	projects, err := m.ListProjects("")
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 {
		t.Fatalf("resource or identity lost: %+v", projects)
	}
	for _, p := range projects {
		if p.Root != first && p.Root != second {
			t.Fatalf("URI not decoded correctly: %+v", p)
		}
	}
	out, err := m.ResolveWorkspace("multi", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved || !out.Ambiguous || len(out.Candidates) != 2 || out.Path != "" {
		t.Fatalf("multiple roots silently selected: %+v", out)
	}
	out, err = m.ResolveWorkspace("alias", "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Path != first {
		t.Fatalf("shared-root registration overwritten: %+v", out)
	}
	again, err := m.ListProjects("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projects, again) {
		t.Fatalf("listing unstable: %+v then %+v", projects, again)
	}
	// Repeated identical resources are one selection, not an artificial ambiguity.
	writeProjectResources(t, config, "c.json", "repeated", "Repeated", second, second)
	out, err = m.ResolveWorkspace("repeated", "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Path != second {
		t.Fatalf("identical resource duplicated: %+v", out)
	}
}

func TestProjectResourcePathsRejectNonlocalURIs(t *testing.T) {
	local := filepath.Join(t.TempDir(), "space %20 #")
	u := projectFileURI(local, "localhost")
	if got, ok := projectResourcePath(u); !ok || got != local {
		t.Fatalf("local URI: %q %t", got, ok)
	}
	for _, input := range []string{"", "relative/path", "https://example.com/path", "file://remote.example/tmp/repo", "file:///tmp/%zz", "file:relative", "file:///tmp/repo?query", "file:///tmp/repo#fragment", "file:///tmp/%00repo", "/tmp/\x00repo"} {
		if got, ok := projectResourcePath(input); ok {
			t.Errorf("unsafe resource %q accepted as %q", input, got)
		}
	}
}

// Mutation guard: ordinary repositories require worktree enumeration too;
// porcelain -z preserves whitespace and newline characters in checkout paths.
func TestProjectOrdinaryRepositoryLinkedWorktrees(t *testing.T) {
	root := canonicalProjectTestRoot(t)
	repo := filepath.Join(root, "main repo")
	createTestGitRepo(t, repo)
	linked := filepath.Join(root, "linked space\ntrailing ") //nolint:gocritic // Exercise a legal POSIX newline and trailing-space filename, not a path separator.
	if runtime.GOOS == "windows" {
		linked = filepath.Join(root, "linked space")
	}
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-b", "feature/topic", linked)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, output)
	}
	config := t.TempDir()
	writeProjectResources(t, config, "repo.json", "ordinary", "ordinary", repo)
	m := &Manager{projectsDir: config, dataDir: t.TempDir()}
	out, err := m.ResolveWorkspace("ordinary", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved || !out.Ambiguous || len(out.Candidates) != 2 {
		t.Fatalf("ordinary linked worktrees lost: %+v", out)
	}
	out, err = m.ResolveWorkspace("ordinary", "feature/topic")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Path != linked || out.Branch != "feature/topic" {
		t.Fatalf("linked path corrupted or unavailable: %+v", out)
	}
}
