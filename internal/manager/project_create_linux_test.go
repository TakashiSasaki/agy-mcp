package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Removing the pre-launch reuse check would invoke AGY again after both a
// successful creation and a lost response with a registration on disk.
func TestCreateProjectReusesNativeRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, exit string
	}{
		{name: "created", exit: "0"},
		{name: "interrupted after writing", exit: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testCloudWorker(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fixtures := t.TempDir()
			const projectID = "new-native-project"
			writeProjectResources(t, fixtures, "fixture.json", projectID, filepath.Base(root), projectFileURI(root, ""))
			calls := filepath.Join(fixtures, "calls")
			t.Setenv("AGY_CREATE_TEST_ROOT", root)
			t.Setenv("AGY_CREATE_TEST_FIXTURE", filepath.Join(fixtures, "fixture.json"))
			t.Setenv("AGY_CREATE_TEST_REGISTRATION", filepath.Join(m.projectsDir, projectID+".json"))
			t.Setenv("AGY_CREATE_TEST_CALLS", calls)
			t.Setenv("AGY_CREATE_TEST_EXIT", tc.exit)
			m.cfg.AgyPath = filepath.Join(fixtures, "agy")
			const script = `#!/bin/sh
set -eu
if [ "$1" = "--version" ]; then
  printf '%s\n' 1.5.3
  exit 0
fi
[ "$#" = 3 ]
[ "$1" = "--new-project" ]
[ "$2" = "--remote-control=false" ]
[ "$3" = "--remote-agent=false" ]
[ "$PWD" = "$AGY_CREATE_TEST_ROOT" ]
[ ! -t 0 ]
printf '%s\n' created >> "$AGY_CREATE_TEST_CALLS"
cp "$AGY_CREATE_TEST_FIXTURE" "$AGY_CREATE_TEST_REGISTRATION"
exit "$AGY_CREATE_TEST_EXIT"
`
			if err := os.WriteFile(m.cfg.AgyPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			created, err := m.CreateProject(t.Context(), root)
			if tc.exit == "0" {
				if err != nil || !created.Created || created.ProjectID != projectID || created.Root != root {
					t.Fatalf("expected new registration: %+v, %v", created, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "retry with the same folder") {
				t.Fatalf("expected uncertain command outcome with retry guidance: %v", err)
			}
			again, err := m.CreateProject(t.Context(), root)
			if err != nil || again.Created || again.ProjectID != projectID || again.Root != root {
				t.Fatalf("retry did not reuse the project: %+v, %v", again, err)
			}
			resolved, err := m.resolveProjectTarget(again.ProjectID, "")
			if err != nil || resolved.Root != root {
				t.Fatalf("job admission could not select the project: %+v, %v", resolved, err)
			}
			b, err := os.ReadFile(calls)
			if err != nil || string(b) != "created\n" {
				t.Fatalf("expected exactly one AGY creation: %q, %v", b, err)
			}
		})
	}
}

// Without the path check, a literal '#' would be stored as a fragment. Without
// the incomplete-scan check, creation could duplicate an unreadable project.
func TestCreateProjectRefusesUnverifiableRegistration(t *testing.T) {
	m, _ := testCloudWorker(t)
	m.cfg.AgyPath = filepath.Join(t.TempDir(), "not-installed")
	unsafe := filepath.Join(t.TempDir(), "path#fragment")
	if err := os.Mkdir(unsafe, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateProject(t.Context(), unsafe); err == nil || !strings.Contains(err.Error(), "cannot safely register") {
		t.Fatalf("unsafe native URI was accepted: %v", err)
	}
	shared := t.TempDir()
	for _, id := range []string{"first", "second"} {
		writeProjectResources(t, m.projectsDir, id+".json", id, id, projectFileURI(shared, ""))
	}
	if _, err := m.CreateProject(t.Context(), shared); err == nil || !strings.Contains(err.Error(), "multiple project registrations") {
		t.Fatalf("ambiguous folder was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(m.projectsDir, "incomplete.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateProject(t.Context(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "cannot verify the project registry") {
		t.Fatalf("incomplete project registry was accepted: %v", err)
	}
}
