package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// loadCache reads agy's conversation cache. A missing file is a normal empty
// cache (fresh agy install, no conversations yet). A read or parse failure is
// reported: agy rewrites the file in place (O_TRUNC, no lock), so a concurrent
// read can be torn, and callers must not mistake a torn read for "no entry"
// when the difference matters.
func loadCache(cacheFile string) (map[string]string, error) {
	b, err := os.ReadFile(cacheFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse conversation cache %s: %w", cacheFile, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("parse conversation cache %s: expected an object, got null", cacheFile)
	}
	return raw, nil
}

// loadCacheRetry reads the cache, retrying once on failure: torn reads are
// transient (agy's rewrite completes in microseconds), so an immediate re-read
// usually observes a complete file.
func loadCacheRetry(cacheFile string) (map[string]string, error) {
	cache, err := loadCache(cacheFile)
	if err == nil {
		return cache, nil
	}
	return loadCache(cacheFile)
}

// resolveLatest distinguishes an absent conversation from an unreadable cache.
// Only absence permits continue_latest to fall back to a fresh conversation.
func resolveLatest(cacheFile, cwd string) (id string, found bool, err error) {
	cache, err := loadCacheRetry(cacheFile)
	if err != nil {
		return "", false, err
	}
	id, ok := cache[cwd]
	return id, ok && id != "", nil
}

// Resolve mutable continuation state only for new work or legacy replay
// comparison. Modern idempotent replays need no readable conversation cache.
func (m *Manager) resolveConversation(req StartRequest) (StartRequest, error) {
	if req.ContinueLatest {
		id, _, err := resolveLatest(m.cacheFile, req.Cwd)
		if err != nil {
			return StartRequest{}, refusedStart("conversation_cache_unavailable", "retry_same_request", true,
				fmt.Errorf("resolve continue_latest: %w; retry when the cache is readable or supply conversation_id explicitly", err))
		}
		req.ConversationID = id
	}
	return req, nil
}

// Note on scope: agy's conversation cache is no longer used to discover the id
// of a run agy-mcp started. The supervisor reads that from agy's own
// stream-json init event. The cache remains a read source for exactly two
// features that ask about conversations agy-mcp did not start: continue_latest
// (resolveLatest, above) and list_sessions (readSessions, in sessions.go).
//
// Watch item, narrowed to those two: both depend on agy continuing to maintain
// last_conversations.json as a cwd->uuid map. agy has been migrating its
// conversation store toward SQLite (1.0.4 made .db "the CLI's conversation
// format"; 1.0.8 added .db/.db-wal scanning to /resume). The JSON file is still
// written as of 1.1.8. If a future agy drops it, continue_latest silently
// starts a fresh conversation and list_sessions returns nothing; jobs
// themselves are unaffected.
