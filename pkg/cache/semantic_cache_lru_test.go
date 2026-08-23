package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
