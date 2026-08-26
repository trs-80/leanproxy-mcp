package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

// maxResultCacheEntries bounds the result cache. On overflow the sweep drops
// expired entries first, then the oldest-expiring live ones.
const maxResultCacheEntries = 256

// resultCache is the exact-match tool-result cache: successful results for
// opt-in tools are replayed for identical (server, tool, arguments) calls
// until their TTL lapses, saving the upstream round trip entirely.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]resultCacheEntry
}

type resultCacheEntry struct {
	result  json.RawMessage
	expires time.Time
}

// SetToolCacheTTL enables exact-match result caching for a tool. Zero or
// negative removes the entry. Only idempotent read-style tools should be
// configured: hits are served verbatim until the TTL lapses.
func (h *Handler) SetToolCacheTTL(serverName, toolName string, ttl time.Duration) {
	key := serverName + "/" + toolName
	if h.toolCacheTTLs == nil {
		h.toolCacheTTLs = make(map[string]time.Duration)
	}
	if ttl <= 0 {
		delete(h.toolCacheTTLs, key)
		return
	}
	h.toolCacheTTLs[key] = ttl
	if h.resultCache == nil {
		h.resultCache = &resultCache{entries: make(map[string]resultCacheEntry)}
	}
}

func resultCacheKey(serverName, toolName string, arguments json.RawMessage) string {
	sum := sha256.Sum256(arguments)
	return serverName + "\x00" + toolName + "\x00" + string(sum[:])
}

// cachedToolResult returns the un-truncated upstream result for an identical
// prior call, if the tool is cache-enabled and the entry is fresh. Callers
// apply response caps afterwards, so per-call max_response_chars still works
// on a hit.
func (h *Handler) cachedToolResult(serverName, toolName string, arguments json.RawMessage) (json.RawMessage, bool) {
	if _, ok := h.toolCacheTTLs[serverName+"/"+toolName]; !ok || h.resultCache == nil {
		return nil, false
	}
	key := resultCacheKey(serverName, toolName, arguments)
	c := h.resultCache
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.result, true
}

// storeToolResult caches a successful upstream result for a cache-enabled
// tool. Results carrying a tool-level execution error (isError) are never
// cached: replaying a transient failure for a whole TTL would turn a blip
// into an outage.
func (h *Handler) storeToolResult(serverName, toolName string, arguments, result json.RawMessage) {
	ttl, ok := h.toolCacheTTLs[serverName+"/"+toolName]
	if !ok || h.resultCache == nil || len(result) == 0 {
		return
	}
	var envelope struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil || envelope.IsError {
		return
	}
	c := h.resultCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxResultCacheEntries {
		c.evictLocked()
	}
	c.entries[resultCacheKey(serverName, toolName, arguments)] = resultCacheEntry{
		result:  result,
		expires: time.Now().Add(ttl),
	}
}

// evictLocked drops expired entries; if none were expired, it drops the
// entry closest to expiry so an insert always finds room.
func (c *resultCache) evictLocked() {
	now := time.Now()
	var oldestKey string
	var oldestExp time.Time
	dropped := false
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
			dropped = true
			continue
		}
		if oldestKey == "" || e.expires.Before(oldestExp) {
			oldestKey, oldestExp = k, e.expires
		}
	}
	if !dropped && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
