// Token accounting for the cached paths.
//
// The savings in BenchmarkSchemaTax_* are measured against payloads built
// in-process. In production those payloads are usually served from a cache
// instead, and nothing checked that a cache round-trip preserves them: a
// dropped or reshaped field would change what the model actually receives
// while every existing benchmark kept reporting the same headline number.
//
// These use reporter.NewEstimator() for the same reason the rest of the file
// does -- it is the primitive pkg/reporter/cost.go uses at runtime, so what
// is asserted here matches what `leanproxy-mcp savings` reports.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/compactor"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/reporter"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/toolstore"
)

// servedToolsJSON is the tools/list result the gateway would emit for these
// tools, which is what the model is billed for.
func servedToolsJSON(tb testing.TB, tools []toolstore.CachedTool) []byte {
	tb.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]any{"tools": tools},
	})
	if err != nil {
		tb.Fatalf("marshal served tools: %v", err)
	}
	return payload
}

// BenchmarkToolCacheTokenParity pins the property the schema-tax numbers
// quietly depend on: what comes back out of the tool cache costs the same
// tokens as what went in. A cold disk read is exercised specifically, since
// the in-memory tier returns the caller's own slice and would hide any
// serialization drift.
func BenchmarkToolCacheTokenParity(b *testing.B) {
	b.Setenv("HOME", b.TempDir())
	estimator := reporter.NewEstimator()

	tools := benchCachedTools()
	coldPayload := servedToolsJSON(b, tools)
	coldTokens := estimator.EstimateTokens(string(coldPayload))

	writer, err := toolstore.NewFileCache(nil)
	if err != nil {
		b.Fatalf("create tool cache: %v", err)
	}
	if err := writer.SetTools("bench-server", tools); err != nil {
		b.Fatalf("SetTools: %v", err)
	}

	// A second cache over the same directory has an empty memory tier, so
	// this read comes off disk the way it would after a restart.
	reader, err := toolstore.NewFileCache(nil)
	if err != nil {
		b.Fatalf("create reader cache: %v", err)
	}
	cached, err := reader.GetTools("bench-server")
	if err != nil {
		b.Fatalf("GetTools: %v", err)
	}
	if len(cached) != len(tools) {
		b.Fatalf("cache round-trip changed tool count: got %d, want %d", len(cached), len(tools))
	}

	cachedPayload := servedToolsJSON(b, cached)
	cachedTokens := estimator.EstimateTokens(string(cachedPayload))
	drift := cachedTokens - coldTokens

	if drift != 0 {
		b.Fatalf("cache round-trip changed the served payload: %d tokens cold, %d cached (drift %+d)",
			coldTokens, cachedTokens, drift)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = estimator.EstimateTokens(string(cachedPayload))
	}
	b.StopTimer()

	// Reported after the loop: b.ResetTimer deletes user metrics, so the
	// natural placement next to the measurement silently drops them.
	b.ReportMetric(float64(coldTokens), "cold_tokens")
	b.ReportMetric(float64(cachedTokens), "cached_tokens")
	b.ReportMetric(float64(drift), "drift_tokens")

	b.Logf("tool cache: tools=%d cold_tokens=%d cached_tokens=%d drift=%+d",
		len(tools), coldTokens, cachedTokens, drift)
}

// BenchmarkDistilledCacheTokenSavings reports the reduction the compactor
// actually delivers from cache -- the number that justifies paying for
// distillation at all -- and fails if a cached manifest serves a different
// payload than the one that was distilled.
func BenchmarkDistilledCacheTokenSavings(b *testing.B) {
	estimator := reporter.NewEstimator()
	dir := b.TempDir()

	rawTools := make([]compactor.RawTool, benchToolCount)
	distilledTools := make([]compactor.DistilledTool, benchToolCount)
	for i := range rawTools {
		name := fmt.Sprintf("tool_%d", i)
		rawTools[i] = compactor.RawTool{
			Name: name,
			Description: "The verbose upstream description an MCP server ships, including " +
				"usage notes, parameter commentary and examples that the distiller exists to strip.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","description":"first"},"b":{"type":"number","description":"second"}},"required":["a"]}`),
		}
		distilledTools[i] = compactor.DistilledTool{
			Name:        name,
			Description: "Short distilled description.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		}
	}

	raw := compactor.RawManifest{Name: "bench-server", Tools: rawTools}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		b.Fatalf("marshal raw manifest: %v", err)
	}
	originalTokens := estimator.EstimateTokens(string(rawJSON))

	manifest := &compactor.DistilledManifest{
		ServerName:   "bench-server",
		OriginalHash: "benchhash",
		Tools:        distilledTools,
		DistilledAt:  time.Now(),
	}
	storedJSON, err := json.Marshal(manifest)
	if err != nil {
		b.Fatalf("marshal distilled manifest: %v", err)
	}
	storedTokens := estimator.EstimateTokens(string(storedJSON))

	ctx := context.Background()
	writer, err := compactor.NewFileCache(dir, nil)
	if err != nil {
		b.Fatalf("create distilled cache: %v", err)
	}
	if err := writer.Set(ctx, "bench-server", manifest); err != nil {
		b.Fatalf("Set: %v", err)
	}

	// Fresh cache over the same directory: forces the disk path.
	reader, err := compactor.NewFileCache(dir, nil)
	if err != nil {
		b.Fatalf("create reader cache: %v", err)
	}
	cached, err := reader.Get(ctx, "bench-server", "benchhash")
	if err != nil {
		b.Fatalf("Get: %v", err)
	}
	if cached == nil {
		b.Fatal("distilled manifest missing from cache")
	}

	cachedJSON, err := json.Marshal(cached)
	if err != nil {
		b.Fatalf("marshal cached manifest: %v", err)
	}
	cachedTokens := estimator.EstimateTokens(string(cachedJSON))
	if cachedTokens != storedTokens {
		b.Fatalf("cache round-trip changed the distilled payload: %d tokens stored, %d cached",
			storedTokens, cachedTokens)
	}

	savings := 1.0 - float64(cachedTokens)/float64(originalTokens)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = estimator.EstimateTokens(string(cachedJSON))
	}
	b.StopTimer()

	// After the loop: b.ResetTimer deletes user metrics.
	b.ReportMetric(float64(originalTokens), "original_tokens")
	b.ReportMetric(float64(cachedTokens), "distilled_tokens")
	b.ReportMetric(savings, "savings_pct")

	b.Logf("distilled cache: tools=%d original_tokens=%d distilled_tokens=%d savings=%.1f%%",
		benchToolCount, originalTokens, cachedTokens, 100*savings)
}
