package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/cache/vectordb"
)

const (
	DefaultSemanticTTL          = 24 * time.Hour
	DefaultEvictionInterval     = 1 * time.Hour
	DefaultStatsPersistInterval = 30 * time.Second
	SemanticSimilarityThreshold = 0.92
	semanticSearchCandidates    = 5
	vectorDeleteTimeout         = 10 * time.Second
	// DefaultMaxEntries bounds the in-memory cache: entries hold full prompts
	// and responses, so TTL alone let a busy proxy accumulate 24h of payloads.
	// When the cap is exceeded the least recently accessed entries are evicted.
	DefaultMaxEntries = 10000
)

type HitType int

const (
	HitMiss HitType = iota
	HitExact
	HitSemantic
)

func (h HitType) String() string {
	switch h {
	case HitExact:
		return "exact"
	case HitSemantic:
		return "semantic"
	default:
		return "miss"
	}
}

type SemanticCacheEntry struct {
	Key       string
	Prompt    string
	ToolName  string
	Response  json.RawMessage
	CreatedAt time.Time
	// accessed is the last-access time in unix nanoseconds, updated
	// atomically so exact-match hits only need the read lock.
	accessed int64
}

// AccessedAt returns the entry's last access time.
func (e *SemanticCacheEntry) AccessedAt() time.Time {
	return time.Unix(0, atomic.LoadInt64(&e.accessed))
}

func (e *SemanticCacheEntry) touch() {
	atomic.StoreInt64(&e.accessed, time.Now().UnixNano())
}

type SemanticCacheResult struct {
	Response   json.RawMessage
	HitType    HitType
	Similarity float64
}

type SemanticCacheStats struct {
	TotalRequests  int64   `json:"total_requests"`
	ExactHits      int64   `json:"exact_hits"`
	SemanticHits   int64   `json:"semantic_hits"`
	Misses         int64   `json:"misses"`
	AvgSimilarity  float64 `json:"avg_similarity"`
	EvictedEntries int64   `json:"evicted_entries"`
}

func (s SemanticCacheStats) HitRate() float64 {
	if s.TotalRequests == 0 {
		return 0.0
	}
	hits := s.ExactHits + s.SemanticHits
	return float64(hits) / float64(s.TotalRequests) * 100
}

func (s SemanticCacheStats) FormatMarkdown() string {
	var b strings.Builder
	b.WriteString("### Semantic Prompt Cache Stats\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	b.WriteString(fmt.Sprintf("| Total Requests | %d |\n", s.TotalRequests))
	b.WriteString(fmt.Sprintf("| Exact Hits | %d |\n", s.ExactHits))
	b.WriteString(fmt.Sprintf("| Semantic Hits | %d |\n", s.SemanticHits))
	b.WriteString(fmt.Sprintf("| Misses | %d |\n", s.Misses))
	b.WriteString(fmt.Sprintf("| Hit Rate | %.2f%% |\n", s.HitRate()))
	b.WriteString(fmt.Sprintf("| Avg Similarity | %.3f |\n", s.AvgSimilarity))
	b.WriteString(fmt.Sprintf("| Evicted Entries | %d |\n", s.EvictedEntries))
	return b.String()
}

func (s SemanticCacheStats) FormatJSON() string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(errData)
	}
	return string(data)
}

// SemanticCache is a tool-scoped prompt cache with exact-match and
// vector-similarity (semantic) lookup, TTL eviction, and periodic stats
// persistence. It is safe for concurrent use.
//
// Cache-aside contract: Get/Set never fail the caller's operation. Vector
// store errors are logged and surfaced as return values where useful, but a
// degraded (or absent) vector store simply means exact-match-only behavior.
type SemanticCache struct {
	mu         sync.RWMutex
	entries    map[string]*SemanticCacheEntry
	maxEntries int
	ttl        time.Duration
	vectorDB   vectordb.Store
	logger     *slog.Logger

	// Counters are atomic so the exact-match read path never needs the write
	// lock; avgSimilarity is only written under mu (semantic-hit path).
	totalRequests atomic.Int64
	exactHits     atomic.Int64
	semanticHits  atomic.Int64
	misses        atomic.Int64
	evicted       atomic.Int64
	avgSimilarity float64

	evictInterval   time.Duration
	persistPath     string
	persistInterval time.Duration

	done    chan struct{}
	started atomic.Bool
	stopped atomic.Bool
	loopWg  sync.WaitGroup // evict/persist loop
	jobsWg  sync.WaitGroup // async vector deletes
}

var globalSemanticCache atomic.Pointer[SemanticCache]

func GlobalSemanticCache() *SemanticCache {
	return globalSemanticCache.Load()
}

func SetGlobalSemanticCache(sc *SemanticCache) {
	globalSemanticCache.Store(sc)
}

// cacheKey scopes cache entries by tool so identical prompts to different
// tools never share an entry.
func cacheKey(toolName, prompt string) string {
	h := sha256.Sum256([]byte(toolName + "\x00" + prompt))
	return hex.EncodeToString(h[:])
}

type SemanticCacheOption func(*SemanticCache)

func WithEvictionInterval(d time.Duration) SemanticCacheOption {
	return func(sc *SemanticCache) {
		if d > 0 {
			sc.evictInterval = d
		}
	}
}

func WithStatsPersistPath(path string) SemanticCacheOption {
	return func(sc *SemanticCache) {
		sc.persistPath = path
	}
}

func WithStatsPersistInterval(d time.Duration) SemanticCacheOption {
	return func(sc *SemanticCache) {
		if d > 0 {
			sc.persistInterval = d
		}
	}
}

// WithMaxEntries caps the number of in-memory entries; n <= 0 disables the
// cap (TTL eviction only).
func WithMaxEntries(n int) SemanticCacheOption {
	return func(sc *SemanticCache) {
		sc.maxEntries = n
	}
}

func NewSemanticCache(vectorDB vectordb.Store, logger *slog.Logger, ttl time.Duration, opts ...SemanticCacheOption) *SemanticCache {
	if logger == nil {
		logger = slog.Default()
	}
	if ttl <= 0 {
		ttl = DefaultSemanticTTL
	}
	sc := &SemanticCache{
		entries:         make(map[string]*SemanticCacheEntry),
		maxEntries:      DefaultMaxEntries,
		ttl:             ttl,
		vectorDB:        vectorDB,
		logger:          logger,
		evictInterval:   DefaultEvictionInterval,
		persistPath:     DefaultSemanticStatsPath(),
		persistInterval: DefaultStatsPersistInterval,
		done:            make(chan struct{}),
	}
	for _, opt := range opts {
		opt(sc)
	}
	return sc
}

// Start launches the background eviction/persistence loop. It is idempotent:
// calling Start more than once is a no-op.
func (sc *SemanticCache) Start(ctx context.Context) {
	if !sc.started.CompareAndSwap(false, true) {
		return
	}
	sc.loopWg.Add(1)
	go sc.runLoop(ctx)
	sc.logger.Debug("semantic cache loop started", "ttl", sc.ttl)
}

// Stop shuts the cache down: it blocks new background work, stops the loop,
// waits for in-flight vector deletes, and writes a final stats snapshot.
// Get/Set remain usable after Stop (the loop simply no longer runs).
func (sc *SemanticCache) Stop() {
	if !sc.started.CompareAndSwap(true, false) {
		return
	}
	sc.mu.Lock()
	sc.stopped.Store(true)
	close(sc.done)
	sc.mu.Unlock()

	sc.loopWg.Wait()
	sc.jobsWg.Wait()
	sc.persistStats()
	sc.logger.Debug("semantic cache stopped")
}

func (sc *SemanticCache) runLoop(ctx context.Context) {
	defer sc.loopWg.Done()
	evictTicker := time.NewTicker(sc.evictInterval)
	persistTicker := time.NewTicker(sc.persistInterval)
	defer evictTicker.Stop()
	defer persistTicker.Stop()

	for {
		select {
		case <-sc.done:
			return
		case <-ctx.Done():
			return
		case <-evictTicker.C:
			sc.evictExpired()
		case <-persistTicker.C:
			sc.persistStats()
		}
	}
}

// Get looks up a cached response for (toolName, prompt). It checks the exact
// key first, then falls back to vector similarity when an embedding is
// available. Lookups never fail the caller: errors degrade to a miss.
func (sc *SemanticCache) Get(ctx context.Context, prompt, toolName string, embedding []float32) (*SemanticCacheResult, error) {
	key := cacheKey(toolName, prompt)
	miss := &SemanticCacheResult{HitType: HitMiss}

	sc.totalRequests.Add(1)

	sc.mu.RLock()
	entry, ok := sc.entries[key]
	usable := ok && sc.entryUsable(entry, toolName)
	if usable {
		entry.touch()
		resp := entry.Response
		sc.mu.RUnlock()
		sc.exactHits.Add(1)
		sc.logger.Info("cache=semantic similarity=1.000",
			"hit_type", "exact",
			"tool", toolName,
			"prompt_hash", key[:12])
		return &SemanticCacheResult{Response: resp, HitType: HitExact, Similarity: 1.0}, nil
	}
	sc.mu.RUnlock()

	if ok {
		// Present but expired: remove it (re-checking under the write lock —
		// a concurrent Set may have replaced it with a fresh entry).
		sc.mu.Lock()
		if current, still := sc.entries[key]; still && current == entry {
			sc.removeEntryLocked(key)
		}
		sc.mu.Unlock()
		sc.misses.Add(1)
		sc.asyncDeleteVector(key)
		return miss, nil
	}

	if sc.vectorDB == nil || len(embedding) == 0 {
		sc.recordMiss()
		return miss, nil
	}

	results, err := sc.vectorDB.Search(ctx, embedding, semanticSearchCandidates)
	if err != nil {
		sc.logger.Warn("semantic cache: vector search failed", "error", err)
		sc.recordMiss()
		return miss, nil
	}

	for _, cand := range results {
		if cand.Score < SemanticSimilarityThreshold {
			continue
		}
		result, staleID, ok := sc.trySemanticHit(cand, toolName)
		if staleID != "" {
			// Deleted after the lock is released: asyncDeleteVector takes
			// sc.mu itself, so spawning it under the lock would just park
			// goroutines on the mutex.
			sc.asyncDeleteVector(staleID)
		}
		if ok {
			return result, nil
		}
	}

	sc.recordMiss()
	return miss, nil
}

// trySemanticHit validates a vector candidate against the in-memory entry
// (presence, tool scope, TTL) under a single lock hold. It returns the result
// and true only when the candidate is fully valid; staleID names a vector
// record the caller should delete (after releasing no locks of its own).
func (sc *SemanticCache) trySemanticHit(cand vectordb.SearchResult, toolName string) (result *SemanticCacheResult, staleID string, ok bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	entry, exists := sc.entries[cand.Record.ID]
	if !exists {
		// Stale vector: present in the store, absent in memory.
		sc.misses.Add(1)
		return nil, cand.Record.ID, false
	}
	if entry.ToolName != toolName {
		// Similar prompt belonging to a different tool — never serve.
		return nil, "", false
	}
	if !sc.entryUsable(entry, toolName) {
		sc.removeEntryLocked(cand.Record.ID)
		sc.misses.Add(1)
		return nil, cand.Record.ID, false
	}

	hits := sc.semanticHits.Add(1)
	n := float64(hits)
	sc.avgSimilarity = (sc.avgSimilarity*(n-1) + cand.Score) / n
	entry.touch()
	resp := entry.Response

	sc.logger.Info(fmt.Sprintf("cache=semantic similarity=%.3f", cand.Score),
		"hit_type", "semantic",
		"tool", toolName,
		"similarity", cand.Score,
		"prompt_hash", cand.Record.ID[:12])

	return &SemanticCacheResult{Response: resp, HitType: HitSemantic, Similarity: cand.Score}, "", true
}

func (sc *SemanticCache) entryUsable(entry *SemanticCacheEntry, toolName string) bool {
	return entry.ToolName == toolName && time.Since(entry.CreatedAt) <= sc.ttl
}

// removeEntryLocked deletes an entry and accounts the eviction.
// Caller must hold sc.mu.
func (sc *SemanticCache) removeEntryLocked(key string) {
	delete(sc.entries, key)
	sc.evicted.Add(1)
}

func (sc *SemanticCache) recordMiss() {
	sc.misses.Add(1)
}

// Set stores a response under the tool-scoped key. An empty response is
// rejected. A vector upsert failure is returned as an error but the
// in-memory entry is still stored (exact-match remains available).
func (sc *SemanticCache) Set(ctx context.Context, prompt string, response json.RawMessage, toolName string, embedding []float32) error {
	if len(response) == 0 {
		return fmt.Errorf("semantic cache: response must not be empty")
	}

	key := cacheKey(toolName, prompt)
	now := time.Now()
	entry := &SemanticCacheEntry{
		Key:       key,
		Prompt:    prompt,
		ToolName:  toolName,
		Response:  response,
		CreatedAt: now,
		accessed:  now.UnixNano(),
	}

	var upsertErr error
	if sc.vectorDB != nil && len(embedding) > 0 {
		rec := vectordb.VectorRecord{
			ID:     key,
			Vector: embedding,
			Metadata: map[string]string{
				"tool_name": toolName,
			},
		}
		if err := sc.vectorDB.Upsert(ctx, rec); err != nil {
			sc.logger.Warn("semantic cache: vector upsert failed", "tool", toolName, "error", err)
			upsertErr = fmt.Errorf("semantic cache: vector upsert: %w", err)
		}
	}

	sc.mu.Lock()
	sc.entries[key] = entry
	var victims []string
	if sc.maxEntries > 0 && len(sc.entries) > sc.maxEntries {
		victims = sc.evictLRULocked(len(sc.entries) - sc.maxEntries + sc.maxEntries/10)
	}
	sc.mu.Unlock()

	if len(victims) > 0 {
		sc.logger.Debug("semantic cache size eviction", "evicted", len(victims), "max_entries", sc.maxEntries)
		sc.asyncDeleteVector(victims...)
	}

	return upsertErr
}

// evictLRULocked removes the n least recently accessed entries and returns
// their keys. Evicting slightly past the cap (the caller adds ~10% headroom)
// amortizes the O(n log n) scan across many inserts. Caller must hold sc.mu.
func (sc *SemanticCache) evictLRULocked(n int) []string {
	if n <= 0 {
		return nil
	}
	type kv struct {
		key      string
		accessed int64
	}
	all := make([]kv, 0, len(sc.entries))
	for key, entry := range sc.entries {
		all = append(all, kv{key, atomic.LoadInt64(&entry.accessed)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].accessed < all[j].accessed })
	if n > len(all) {
		n = len(all)
	}
	victims := make([]string, 0, n)
	for _, item := range all[:n] {
		delete(sc.entries, item.key)
		victims = append(victims, item.key)
	}
	sc.evicted.Add(int64(n))
	return victims
}

func (sc *SemanticCache) PurgeTool(toolName string) int {
	sc.mu.Lock()
	var ids []string
	for key, entry := range sc.entries {
		if entry.ToolName == toolName {
			ids = append(ids, key)
			delete(sc.entries, key)
		}
	}
	sc.mu.Unlock()

	sc.asyncDeleteVector(ids...)

	if len(ids) > 0 {
		sc.logger.Info("semantic cache purged", "tool", toolName, "count", len(ids))
	}
	return len(ids)
}

func (sc *SemanticCache) PurgeAll() int {
	sc.mu.Lock()
	count := len(sc.entries)
	ids := make([]string, 0, count)
	for key := range sc.entries {
		ids = append(ids, key)
	}
	sc.entries = make(map[string]*SemanticCacheEntry)
	sc.mu.Unlock()

	sc.asyncDeleteVector(ids...)

	if count > 0 {
		sc.logger.Info("semantic cache purged all", "count", count)
	}
	return count
}

func (sc *SemanticCache) Stats() SemanticCacheStats {
	sc.mu.RLock()
	avg := sc.avgSimilarity
	sc.mu.RUnlock()
	return SemanticCacheStats{
		TotalRequests:  sc.totalRequests.Load(),
		ExactHits:      sc.exactHits.Load(),
		SemanticHits:   sc.semanticHits.Load(),
		Misses:         sc.misses.Load(),
		AvgSimilarity:  avg,
		EvictedEntries: sc.evicted.Load(),
	}
}

func (sc *SemanticCache) Len() int {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return len(sc.entries)
}

func (sc *SemanticCache) evictExpired() {
	sc.mu.Lock()
	now := time.Now()
	var victims []string
	for key, entry := range sc.entries {
		if now.Sub(entry.CreatedAt) > sc.ttl {
			victims = append(victims, key)
			delete(sc.entries, key)
		}
	}
	if len(victims) > 0 {
		sc.evicted.Add(int64(len(victims)))
	}
	sc.mu.Unlock()

	if len(victims) > 0 {
		sc.logger.Debug("semantic cache eviction", "evicted", len(victims))
		sc.asyncDeleteVector(victims...)
	}
}

// asyncDeleteVector deletes vector records in the background. The goroutine
// is tracked so Stop() waits for it, and it is suppressed once the cache is
// stopped to avoid racing vector-store Close during shutdown.
func (sc *SemanticCache) asyncDeleteVector(ids ...string) {
	if sc.vectorDB == nil || len(ids) == 0 {
		return
	}
	sc.mu.Lock()
	if sc.stopped.Load() {
		sc.mu.Unlock()
		return
	}
	sc.jobsWg.Add(1)
	sc.mu.Unlock()

	go func() {
		defer sc.jobsWg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), vectorDeleteTimeout)
		defer cancel()
		if err := sc.vectorDB.Delete(ctx, ids...); err != nil {
			sc.logger.Warn("semantic cache: vector delete failed", "count", len(ids), "error", err)
		}
	}()
}
