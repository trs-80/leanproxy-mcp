package vectordb

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
)

// TestSearchManualTopKOrdering verifies the streaming top-k selection returns
// exactly the k best-scoring records in descending order, with more records
// in the store than k.
func TestSearchManualTopKOrdering(t *testing.T) {
	store, err := newSQLiteStore(&migrate.SQLiteVectorConfig{Path: filepath.Join(t.TempDir(), "v.db")}, 3, slog.Default())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Records at increasing angles from the query vector: r0 closest.
	vecs := [][]float32{
		{1, 0, 0},
		{0.9, 0.1, 0},
		{0.7, 0.3, 0},
		{0.5, 0.5, 0},
		{0.2, 0.8, 0},
		{0, 1, 0},
		{0, 0.5, 0.5},
		{0, 0, 1},
	}
	for i, v := range vecs {
		if err := store.Upsert(ctx, VectorRecord{ID: fmt.Sprintf("r%d", i), Vector: v, Metadata: map[string]string{"n": fmt.Sprint(i)}}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	results, err := store.Search(ctx, []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	for i, wantID := range []string{"r0", "r1", "r2"} {
		if results[i].Record.ID != wantID {
			t.Errorf("results[%d] = %s (score %.3f), want %s", i, results[i].Record.ID, results[i].Score, wantID)
		}
		if results[i].Record.Metadata["n"] != fmt.Sprint(i) {
			t.Errorf("results[%d] metadata lost: %v", i, results[i].Record.Metadata)
		}
	}
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not in descending score order: %v > %v", results[i].Score, results[i-1].Score)
		}
	}
}

// BenchmarkSearchManualRealistic measures the brute-force fallback at a
// realistic corpus size and embedding dimension (the vec0 fast path cannot
// load under the pure-Go sqlite driver, so production always runs this).
func BenchmarkSearchManualRealistic(b *testing.B) {
	for _, tc := range []struct {
		rows, dim int
	}{
		{1000, 768},
		{5000, 768},
		{5000, 1536},
	} {
		b.Run(fmt.Sprintf("rows=%d_dim=%d", tc.rows, tc.dim), func(b *testing.B) {
			store, err := newSQLiteStore(&migrate.SQLiteVectorConfig{Path: ":memory:"}, tc.dim, slog.New(slog.DiscardHandler))
			if err != nil {
				b.Fatalf("store: %v", err)
			}
			defer store.Close()
			ctx := context.Background()
			vec := make([]float32, tc.dim)
			for i := 0; i < tc.rows; i++ {
				for j := range vec {
					vec[j] = float32((i*31+j*17)%97) / 97
				}
				if err := store.Upsert(ctx, VectorRecord{ID: fmt.Sprintf("r%d", i), Vector: vec}); err != nil {
					b.Fatalf("upsert: %v", err)
				}
			}
			query := make([]float32, tc.dim)
			for j := range query {
				query[j] = float32(j%97) / 97
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.Search(ctx, query, 5); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestSearchTopKEdgeCases covers the boundary shapes of the top-k selection:
// k larger than the record count, k of zero, an empty store, and tie scores.
func TestSearchTopKEdgeCases(t *testing.T) {
	newStore := func(t *testing.T) *sqliteStore {
		t.Helper()
		store, err := newSQLiteStore(&migrate.SQLiteVectorConfig{Path: filepath.Join(t.TempDir(), "v.db")}, 3, slog.Default())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
	ctx := context.Background()

	t.Run("k greater than record count returns all records", func(t *testing.T) {
		store := newStore(t)
		for i, v := range [][]float32{{1, 0, 0}, {0, 1, 0}} {
			if err := store.Upsert(ctx, VectorRecord{ID: fmt.Sprintf("r%d", i), Vector: v}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
		results, err := store.Search(ctx, []float32{1, 0, 0}, 10)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("want all 2 records for k=10, got %d", len(results))
		}
	})

	t.Run("k of zero falls back to the default k", func(t *testing.T) {
		// Contract: Search treats k <= 0 as "use the default" (10), so a
		// non-positive k still returns available records rather than nothing.
		store := newStore(t)
		if err := store.Upsert(ctx, VectorRecord{ID: "r0", Vector: []float32{1, 0, 0}}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		results, err := store.Search(ctx, []float32{1, 0, 0}, 0)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("want the 1 stored record via default k, got %d", len(results))
		}
	})

	t.Run("empty store returns no results", func(t *testing.T) {
		store := newStore(t)
		results, err := store.Search(ctx, []float32{1, 0, 0}, 5)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("want 0 results from empty store, got %d", len(results))
		}
	})

	t.Run("tie scores keep k results without duplication", func(t *testing.T) {
		store := newStore(t)
		// Three identical vectors (identical scores) plus one orthogonal.
		for i, v := range [][]float32{{1, 0, 0}, {1, 0, 0}, {1, 0, 0}, {0, 0, 1}} {
			if err := store.Upsert(ctx, VectorRecord{ID: fmt.Sprintf("r%d", i), Vector: v}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
		results, err := store.Search(ctx, []float32{1, 0, 0}, 2)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("want 2 results, got %d", len(results))
		}
		seen := map[string]bool{}
		for _, r := range results {
			if seen[r.Record.ID] {
				t.Errorf("duplicate result %s in tie-break", r.Record.ID)
			}
			seen[r.Record.ID] = true
			if r.Record.ID == "r3" {
				t.Errorf("orthogonal record outranked a perfect-score tie: %v", results)
			}
		}
	})
}
