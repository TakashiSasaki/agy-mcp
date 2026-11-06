package manager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

func writeCache(t *testing.T, dir string, kv map[string]string) string {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveLatestForCwd(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, map[string]string{"/w": "uuid-existing"})
	id, ok, err := resolveLatest(cache, "/w")
	if err != nil || !ok || id != "uuid-existing" {
		t.Fatalf("resolveLatest = %q,%v,%v", id, ok, err)
	}
	if _, ok, err := resolveLatest(cache, "/missing"); err != nil || ok {
		t.Fatalf("missing cwd should not resolve or fail: %v", err)
	}
}

func TestLoadCacheMissingFileIsEmptyNotError(t *testing.T) {
	cache, err := loadCache(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing cache is a normal empty cache, got error: %v", err)
	}
	if len(cache) != 0 {
		t.Fatalf("want empty cache, got %v", cache)
	}
}

func TestLoadCacheTornReadIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCache(p); err == nil {
		t.Fatal("a torn/unparsable cache read must surface an error")
	}
}

// Mutation guard: resolveLatest must propagate read errors instead of treating
// them as absence, which would authorize a fresh conversation accidentally.
func TestResolveLatestReportsTornCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLatest(p, "/w"); err == nil || ok {
		t.Fatal("a torn cache must report an error, not absence or a bogus id")
	}
}

func TestNewUsesConfiguredCacheFile(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 1,
		ConversationCacheFile: "/tmp/agy-test-cache.json"})
	if m.cacheFile != "/tmp/agy-test-cache.json" {
		t.Fatalf("cacheFile = %q, want the configured override", m.cacheFile)
	}
}

// Mutation guards: modern replay precedes cache resolution, legacy argument
// replay requires it, and a refusal releases the idempotency creation claim.
func TestContinueLatestReplayWithUnreadableCache(t *testing.T) {
	if !proc.Supported {
		t.Skip("job supervision unsupported on this platform")
	}
	for _, legacy := range []bool{false, true} {
		m := newManager(t, managerOpts{withCacheFile: true, defaultTimeout: time.Minute})
		req := StartRequest{Prompt: "continue", Cwd: t.TempDir(), ContinueLatest: true, IdempotencyKey: "original"}
		normalized, err := m.normalizeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		normalized.ConversationID = "original-conversation"
		meta := jobstore.Meta{ID: "already-created", PID: -1, StartedAt: time.Now(), Cwd: normalized.Cwd, ConversationID: normalized.ConversationID,
			Args: buildAgyArgs(normalized), IdempotencyKey: req.IdempotencyKey, RequestKey: requestKey(normalized)}
		if legacy {
			meta.RequestKey = ""
		}
		if _, err := m.store.Create(meta); err != nil {
			t.Fatal(err)
		}
		if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.cacheFile, []byte(`{"torn`), 0o600); err != nil {
			t.Fatal(err)
		}
		job, err := m.StartJob(req)
		if legacy {
			start, ok := errors.AsType[*StartError](err)
			if !ok || start.Code != "conversation_cache_unavailable" {
				t.Fatalf("legacy replay guessed the conversation: %+v %v", job, err)
			}
			// A later readable cache permits that same legacy replay.
			writeCache(t, filepath.Dir(m.cacheFile), map[string]string{normalized.Cwd: normalized.ConversationID})
			job, err = m.StartJob(req)
		}
		if err != nil || job.ID != meta.ID || job.ConversationID != normalized.ConversationID {
			t.Fatalf("existing job was lost during replay, legacy=%v: %+v %v", legacy, job, err)
		}
		if err := os.WriteFile(m.cacheFile, []byte(`{"torn`), 0o600); err != nil {
			t.Fatal(err)
		}
		req.IdempotencyKey = "new-work"
		for range 2 {
			_, err := m.StartJob(req)
			start, ok := errors.AsType[*StartError](err)
			if !ok || start.Code != "conversation_cache_unavailable" || start.JobStarted == nil || *start.JobStarted {
				t.Fatalf("new work did not fail safely or leaked its claim: %v", err)
			}
		}
		ids, err := m.store.List()
		if err != nil || len(ids) != 1 || ids[0] != meta.ID {
			t.Fatalf("replay/refusal changed retained jobs: %v %v", ids, err)
		}
	}
}
