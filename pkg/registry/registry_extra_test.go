package registry

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadTwiceDoesNotDuplicateIndexes is the regression test for the index
// slices growing duplicates on every Load, which made FindByCapability
// return the same server multiple times.
func TestLoadTwiceDoesNotDuplicateIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	ctx := context.Background()

	r := NewRegistry(slog.Default(), path)
	if err := r.Register(ctx, ServerEntry{ID: "s1", Transport: TransportStdio, Capabilities: []string{"tools"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Save(ctx); err != nil {
		t.Fatalf("save: %v", err)
	}

	r2 := NewRegistry(slog.Default(), path)
	if err := r2.Load(ctx); err != nil {
		t.Fatalf("load 1: %v", err)
	}
	if err := r2.Load(ctx); err != nil {
		t.Fatalf("load 2: %v", err)
	}

	byCap, err := r2.FindByCapability(ctx, "tools")
	if err != nil {
		t.Fatalf("FindByCapability: %v", err)
	}
	if len(byCap) != 1 {
		t.Fatalf("duplicate index entries after re-Load: got %d results, want 1", len(byCap))
	}
	byTrans, err := r2.FindByTransport(ctx, TransportStdio)
	if err != nil {
		t.Fatalf("FindByTransport: %v", err)
	}
	if len(byTrans) != 1 {
		t.Fatalf("duplicate transport entries after re-Load: got %d results, want 1", len(byTrans))
	}
	_ = os.Remove(path)
}
