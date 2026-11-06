//go:build linux || darwin

package manager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// Mutation guards: resolving a cached ID must still control the spawned args
// and persisted metadata; missing files or entries still permit fresh work.
func TestContinueLatestStartsResolvedConversation(t *testing.T) {
	for _, name := range []string{"missing file", "missing workspace", "existing conversation"} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", withCacheFile: true, defaultTimeout: time.Minute,
				supervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"})})
			cwd, err := normalizeCwd(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if name != "missing file" {
				cache := map[string]string{}
				if name == "existing conversation" {
					want = "cached-conversation"
					cache[cwd] = want
				}
				writeCache(t, filepath.Dir(m.cacheFile), cache)
			}
			job, err := m.StartJob(StartRequest{Prompt: "continue", Cwd: cwd, ContinueLatest: true})
			if err != nil {
				t.Fatal(err)
			}
			deferJobDone(t, m, job.ID)
			meta, err := m.store.Load(job.ID)
			if err != nil || meta.ConversationID != want || job.ConversationID != want {
				t.Fatalf("wrong resumed context: job=%+v meta=%+v error=%v", job, meta, err)
			}
			if want == "" && contains(meta.Args, "--conversation") || want != "" && !hasArg(meta.Args, "--conversation", want) {
				t.Fatalf("wrong CLI continuation arguments: %v", meta.Args)
			}
		})
	}
}
