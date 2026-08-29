// Cache write-path benchmarks.
//
// These exist because a 39x regression in the on-disk cache write path
// shipped invisibly: the rest of this suite exercises token accounting and
// request overhead, so nothing here touched pkg/toolstore or pkg/compactor,
// and `make bench` stayed flat while every cache write got ~5.8ms slower.
//
// Both caches persist once per configured server, and the tool cache does so
// before the listener opens (cmd/serve.go calls PopulateToolCache ahead of
// net.Listen), so per-write cost lands directly on startup latency and on the
// first cold tools/list a client sees.
//
// Reference points measured on an Apple M4 Max, 100-tool manifest, count=6:
//
//	plain os.WriteFile (pre-atomic)   149us
//	temp file + fsync + rename       5839us   <- the regression
//	temp file + rename (current)      237us
//
// The ~59% over plain WriteFile is the deliberate price of atomicity: a
// reader never observes a half-written cache file. What this guards against
// is another order-of-magnitude jump going unnoticed.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/internal/cachefile"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/compactor"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/toolstore"
)

// benchToolCount matches the largest server in the live snapshot (garmin,
// 100 tools), so the payload is the worst realistic case rather than a toy.
const benchToolCount = 100

func benchCachedTools() []toolstore.CachedTool {
	tools := make([]toolstore.CachedTool, benchToolCount)
	for i := range tools {
		tools[i] = toolstore.CachedTool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "A representative tool description of roughly the length real MCP servers ship.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}`),
		}
	}
	return tools
}

// BenchmarkToolCacheWrite covers pkg/toolstore end to end: marshal plus the
// atomic replace, which is what PopulateToolCache pays per server.
func BenchmarkToolCacheWrite(b *testing.B) {
	// NewFileCache resolves its directory from $HOME, so redirect it rather
	// than writing into the developer's real config directory.
	b.Setenv("HOME", b.TempDir())

	cache, err := toolstore.NewFileCache(nil)
	if err != nil {
		b.Fatalf("create tool cache: %v", err)
	}
	tools := benchCachedTools()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cache.SetTools("bench-server", tools); err != nil {
			b.Fatalf("SetTools: %v", err)
		}
	}
}

// BenchmarkDistilledCacheWrite covers the same path in pkg/compactor, whose
// manifests are the ones a wrongly-invalidated entry re-bills an LLM call to
// rebuild.
func BenchmarkDistilledCacheWrite(b *testing.B) {
	cache, err := compactor.NewFileCache(b.TempDir(), nil)
	if err != nil {
		b.Fatalf("create distilled cache: %v", err)
	}

	tools := make([]compactor.DistilledTool, benchToolCount)
	for i := range tools {
		tools[i] = compactor.DistilledTool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "A distilled description, shorter than the original by design.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		}
	}
	manifest := &compactor.DistilledManifest{
		ServerName:   "bench-server",
		OriginalHash: "benchhash",
		Tools:        tools,
		DistilledAt:  time.Now(),
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cache.Set(ctx, "bench-server", manifest); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

// BenchmarkCacheFileWriteAtomic isolates the shared primitive, so a change in
// the numbers above can be attributed to the write mechanics rather than to
// marshaling or cache bookkeeping.
func BenchmarkCacheFileWriteAtomic(b *testing.B) {
	payload, err := json.Marshal(benchCachedTools())
	if err != nil {
		b.Fatalf("marshal payload: %v", err)
	}
	b.Logf("payload: %d bytes", len(payload))

	path := b.TempDir() + "/cache.json"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cachefile.WriteAtomic(path, payload, cachefile.FilePerm); err != nil {
			b.Fatalf("WriteAtomic: %v", err)
		}
	}
}
