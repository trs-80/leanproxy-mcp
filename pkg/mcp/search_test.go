package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Equal-score matches must render in server-then-tool order on every call:
// the cache is a map, and any iteration-order leak into the output shuffles
// the prefix across sessions and defeats provider prompt caching.
func TestSearchTools_EqualScoresSortByServerThenTool(t *testing.T) {
	t.Parallel()

	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "zeta", Tool{Name: "sync_b", Description: "", InputSchema: json.RawMessage(`{}`)})
	seedToolCache(h, "alpha", Tool{Name: "sync_b", Description: "", InputSchema: json.RawMessage(`{}`)},
		Tool{Name: "sync_a", Description: "", InputSchema: json.RawMessage(`{}`)})

	want := []string{"alpha_sync_a: ", "alpha_sync_b: ", "zeta_sync_b: "}
	for i := 0; i < 10; i++ {
		got := h.searchToolCacheFiltered("sync", "", 120)
		require.Equal(t, want, got, "iteration %d produced a different order", i)
	}
}

// An empty query is a browse: every cached tool comes back, still in
// deterministic server-then-tool order.
func TestSearchTools_EmptyQueryListsEverythingSorted(t *testing.T) {
	t.Parallel()

	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "gh", Tool{Name: "list_issues", InputSchema: json.RawMessage(`{}`)})
	seedToolCache(h, "cbm", Tool{Name: "trace_path", InputSchema: json.RawMessage(`{}`)})

	got := h.searchToolCacheFiltered("", "", 120)

	require.Len(t, got, 2)
	assert.True(t, strings.HasPrefix(got[0], "cbm_trace_path"))
	assert.True(t, strings.HasPrefix(got[1], "gh_list_issues"))
}

// Name hits outrank description hits regardless of which server the tool
// lives on: ranking is about relevance, not cache layout.
func TestSearchTools_NameMatchOutranksDescriptionMatch(t *testing.T) {
	t.Parallel()

	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "aaa", Tool{Name: "unrelated", Description: "does a deploy", InputSchema: json.RawMessage(`{}`)})
	seedToolCache(h, "zzz", Tool{Name: "deploy_app", Description: "", InputSchema: json.RawMessage(`{}`)})

	got := h.searchToolCacheFiltered("deploy", "", 120)

	require.Len(t, got, 2)
	assert.True(t, strings.HasPrefix(got[0], "zzz_deploy_app"), "name match must rank first, got %q", got[0])
}

// A single full-coverage match must not silence the tool the query is
// actually about. Reproduces a live A/B sweep failure: the model asked
// search_tools for "search graph trace callers", got back exactly one tool
// (search_graph, which happened to hit all four words), never saw trace_path,
// and burned four turns without ever reaching the tool it needed.
//
// Word hits below mirror the real codebase-memory cache that produced it:
// search_graph scores 4/4 (search+graph in the name, trace+callers in the
// description) and clears the full-coverage bonus; trace_path scores 3/4,
// missing only "search".
func TestSearchTools_OneFullMatchDoesNotHidePartialMatches(t *testing.T) {
	t.Parallel()

	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "cbm",
		Tool{
			Name:        "search_graph",
			Description: "Search the code knowledge graph. Use INSTEAD OF grep when finding definitions; can trace relationships and callers.",
			InputSchema: json.RawMessage(`{}`),
		},
		Tool{
			Name:        "trace_path",
			Description: "Trace paths through the code graph. Modes: calls (callers/callees), data_flow, cross_service.",
			InputSchema: json.RawMessage(`{}`),
		})

	got := h.searchToolCacheFiltered("search graph trace callers", "", 120)

	joined := strings.Join(got, "\n")
	assert.Contains(t, joined, "cbm_trace_path",
		"trace_path (3 of 4 query words) was dropped because search_graph alone matched all four")
	require.NotEmpty(t, got)
	assert.True(t, strings.HasPrefix(got[0], "cbm_search_graph"),
		"the full-coverage match must still rank first, got %q", got[0])
}

func BenchmarkSearchToolCacheFiltered(b *testing.B) {
	h := NewHandler(newMockPool(), nil)
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
	for s := 0; s < 5; s++ {
		var tools []Tool
		for i := 0; i < 40; i++ {
			tools = append(tools, Tool{
				Name:        fmt.Sprintf("tool_%d_search", i),
				Description: "searches things in the indexed graph and returns matches",
				InputSchema: schema,
			})
		}
		h.toolCache.tools[fmt.Sprintf("server%d", s)] = tools
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.searchToolCacheFiltered("search graph", "", 120)
	}
}
