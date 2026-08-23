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
