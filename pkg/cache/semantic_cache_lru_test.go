package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestSemanticCacheMaxEntriesEvictsLRU verifies the size cap evicts the least
// recently accessed entries and keeps recently touched ones.
func TestSemanticCacheMaxEntriesEvictsLRU(t *testing.T) {
	sc := NewSemanticCache(nil, slog.Default(), time.Hour, WithMaxEntries(10))
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := sc.Set(ctx, fmt.Sprintf("prompt-%d", i), json.RawMessage(`"r"`), "tool", nil); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	// Touch entries 0-4 so they are the most recently accessed.
	for i := 0; i < 5; i++ {
		if res, _ := sc.Get(ctx, fmt.Sprintf("prompt-%d", i), "tool", nil); res.HitType != HitExact {
			t.Fatalf("warm Get(prompt-%d) missed", i)
		}
	}

	// One more insert exceeds the cap of 10 and evicts cap/10+1 = 2 oldest.
	if err := sc.Set(ctx, "prompt-new", json.RawMessage(`"r"`), "tool", nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if n := sc.Len(); n > 10 {
		t.Fatalf("cache over cap after eviction: %d entries", n)
	}
	// Recently touched entries and the new entry must survive.
	for _, p := range []string{"prompt-0", "prompt-4", "prompt-new"} {
		if res, _ := sc.Get(ctx, p, "tool", nil); res.HitType != HitExact {
			t.Errorf("recently used %q was evicted", p)
		}
	}
	if sc.Stats().EvictedEntries == 0 {
		t.Error("eviction not accounted in stats")
	}
}

// TestSemanticCacheUnboundedWhenDisabled keeps the opt-out contract.
func TestSemanticCacheUnboundedWhenDisabled(t *testing.T) {
	sc := NewSemanticCache(nil, slog.Default(), time.Hour, WithMaxEntries(0))
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		_ = sc.Set(ctx, fmt.Sprintf("p-%d", i), json.RawMessage(`"r"`), "tool", nil)
	}
	if n := sc.Len(); n != 50 {
		t.Fatalf("expected 50 entries with cap disabled, got %d", n)
	}
}

// TestSemanticCacheConcurrentGetSetAtCap hammers Get and Set at the
// max-entries boundary so LRU eviction races concurrent readers; under -race
// this guards the RLock/atomic access-time design around eviction.
func TestSemanticCacheConcurrentGetSetAtCap(t *testing.T) {
	const cap = 8
	sc := NewSemanticCache(nil, discardLogger(), time.Hour, WithMaxEntries(cap))
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("prompt-%d", (w*200+i)%(2*cap))
				if err := sc.Set(ctx, key, json.RawMessage(`{"ok":true}`), "tool", nil); err != nil {
					t.Errorf("Set(%s): %v", key, err)
					return
				}
				res, err := sc.Get(ctx, key, "tool", nil)
				if err != nil {
					t.Errorf("Get(%s): %v", key, err)
					return
				}
				// The entry may have been evicted by a concurrent Set (miss is
				// legal); a hit must carry the stored payload.
				if res.HitType == HitExact && string(res.Response) != `{"ok":true}` {
					t.Errorf("Get(%s) returned corrupted payload %s", key, res.Response)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if n := sc.Len(); n > cap {
		t.Errorf("cap %d exceeded after concurrent churn: %d entries", cap, n)
	}
}

// TestSemanticCacheSetExistingKeyAtCapDoesNotEvict: overwriting a key while
// the cache sits exactly at the cap must update in place, not trigger an
// eviction of an unrelated entry.
func TestSemanticCacheSetExistingKeyAtCapDoesNotEvict(t *testing.T) {
	sc := NewSemanticCache(nil, discardLogger(), time.Hour, WithMaxEntries(4))
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := sc.Set(ctx, fmt.Sprintf("p%d", i), json.RawMessage(`"v1"`), "tool", nil); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	evictedBefore := sc.Stats().EvictedEntries

	if err := sc.Set(ctx, "p0", json.RawMessage(`"v2"`), "tool", nil); err != nil {
		t.Fatalf("re-Set: %v", err)
	}

	if n := sc.Len(); n != 4 {
		t.Errorf("re-Set of existing key changed entry count: %d, want 4", n)
	}
	if evicted := sc.Stats().EvictedEntries; evicted != evictedBefore {
		t.Errorf("re-Set of existing key evicted entries: %d -> %d", evictedBefore, evicted)
	}
	res, err := sc.Get(ctx, "p0", "tool", nil)
	if err != nil || res.HitType != HitExact || string(res.Response) != `"v2"` {
		t.Errorf("re-Set did not update value: hit=%v resp=%s err=%v", res.HitType, res.Response, err)
	}
	for i := 1; i < 4; i++ {
		if res, _ := sc.Get(ctx, fmt.Sprintf("p%d", i), "tool", nil); res.HitType != HitExact {
			t.Errorf("unrelated entry p%d lost after re-Set of p0", i)
		}
	}
}
