//go:build linux

package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

func TestStartJobPassesSupervisorAdmissionPipe(t *testing.T) {
	dir := t.TempDir()
	supervisor := filepath.Join(dir, "admission-sup")
	// A supervisor launched by the real manager must receive a private
	// pipe. On EOF the manager's published PID must already match this child.
	script := fmt.Sprintf(`#!/bin/bash
code=1
if [[ $1 = run-job && $3 = --await-admission && -p /dev/stdin ]] &&
   ! IFS= read -r unexpected &&
   grep -qF "\"pid\": $$," "$2/%s"; then
    code=0
fi
printf '%%s' "$code" > "$2/%s"
`, jobstore.MetaFile, jobstore.ExitCodeFile)
	if err := os.WriteFile(supervisor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: supervisor})
	job, err := m.StartJob(StartRequest{Prompt: "admission", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	var code int
	testutil.WaitFor(t, 5*time.Second, func() bool {
		var found bool
		code, found = m.store.ExitCode(job.ID)
		return found
	}, "supervisor did not finish")
	if code != 0 {
		t.Fatalf("supervisor did not observe admission after its PID was recorded, exit %d", code)
	}
}
