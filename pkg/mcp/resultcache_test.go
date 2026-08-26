package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingPool wraps mockPool behavior with an upstream tools/call counter so
// tests can assert exactly how many calls escaped the cache.
func newCountingCachePool(result string) (*mockPool, *int) {
	calls := 0
	mp := newMockPool()
	mp.servers["cbm"] = "idle"
	mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
		if method == MethodToolsCall {
			calls++
		}
		res, _ := json.Marshal(map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": result}},
		})
		return &pool.Response{Result: res}, nil
	}
	return mp, &calls
}

func invokeListProjects(t *testing.T, h *Handler, args map[string]interface{}) *Response {
	t.Helper()
	call := map[string]interface{}{"server": "cbm", "tool": "list_projects"}
	if args != nil {
		call["arguments"] = args
	}
	return callGatewayTool(t, h, "invoke_tool", call)
}

// An identical repeated call must be served from the cache: one upstream
// round trip, byte-identical result.
func TestResultCache_IdenticalCallHitsCache(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool("projects: a, b")
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Minute)

	first := invokeListProjects(t, h, map[string]interface{}{"scope": "all"})
	second := invokeListProjects(t, h, map[string]interface{}{"scope": "all"})

	require.Nil(t, first.Error)
	require.Nil(t, second.Error)
	assert.Equal(t, 1, *calls, "second identical call must not reach the upstream")
	assert.JSONEq(t, string(first.Result), string(second.Result))
}

// Different arguments are different cache entries.
func TestResultCache_DifferentArgumentsMiss(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool("ok")
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Minute)

	invokeListProjects(t, h, map[string]interface{}{"scope": "all"})
	invokeListProjects(t, h, map[string]interface{}{"scope": "mine"})

	assert.Equal(t, 2, *calls)
}

// A tool without a configured TTL is never cached.
func TestResultCache_UnconfiguredToolNotCached(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool("ok")
	h := NewHandler(mp, nil)

	invokeListProjects(t, h, nil)
	invokeListProjects(t, h, nil)

	assert.Equal(t, 2, *calls)
}

// An expired entry is refetched.
func TestResultCache_ExpiredEntryRefetches(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool("ok")
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", 10*time.Millisecond)

	invokeListProjects(t, h, nil)
	time.Sleep(25 * time.Millisecond)
	invokeListProjects(t, h, nil)

	assert.Equal(t, 2, *calls)
}

// The direct tools/call path and invoke_tool share entries: keys are the
// stripped arguments, independent of dispatch surface.
func TestResultCache_SharedAcrossDispatchPaths(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool("shared")
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Minute)

	directArgs, err := json.Marshal(map[string]interface{}{"name": "cbm_list_projects", "arguments": map[string]interface{}{"scope": "all"}})
	require.NoError(t, err)
	direct, err := h.handleToolsCall(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: directArgs})
	require.NoError(t, err)
	require.Nil(t, direct.Error)

	viaInvoke := invokeListProjects(t, h, map[string]interface{}{"scope": "all"})

	require.Nil(t, viaInvoke.Error)
	assert.Equal(t, 1, *calls, "invoke_tool must hit the entry stored by tools/call")
}

// Results carrying a tool-level execution error are never cached: a transient
// failure must not be replayed for a whole TTL.
func TestResultCache_IsErrorResultNotCached(t *testing.T) {
	t.Parallel()

	calls := 0
	mp := newMockPool()
	mp.servers["cbm"] = "idle"
	mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
		if method == MethodToolsCall {
			calls++
		}
		res, _ := json.Marshal(map[string]interface{}{
			"isError": true,
			"content": []map[string]string{{"type": "text", "text": "backend unavailable"}},
		})
		return &pool.Response{Result: res}, nil
	}
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Minute)

	invokeListProjects(t, h, nil)
	invokeListProjects(t, h, nil)

	assert.Equal(t, 2, calls)
}

// A cache hit still honors a per-call max_response_chars: the cache stores
// the full upstream result and caps are applied on the way out.
func TestResultCache_HitStillAppliesExplicitCap(t *testing.T) {
	t.Parallel()

	mp, calls := newCountingCachePool(strings.Repeat("x", 5000))
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Minute)

	full := invokeListProjects(t, h, nil)
	require.Nil(t, full.Error)
	require.NotContains(t, string(full.Result), "truncated")

	capped := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{
		"server": "cbm", "tool": "list_projects", "max_response_chars": 1000,
	})

	require.Nil(t, capped.Error)
	assert.Equal(t, 1, *calls, "capped call must be served from cache")
	assert.Contains(t, string(capped.Result), "truncated, 1000 of 5000 chars shown")
}

// The cache never grows past its bound.
func TestResultCache_EvictionBoundsSize(t *testing.T) {
	t.Parallel()

	mp, _ := newCountingCachePool("ok")
	h := NewHandler(mp, nil)
	h.SetToolCacheTTL("cbm", "list_projects", time.Hour)

	for i := 0; i < maxResultCacheEntries+20; i++ {
		invokeListProjects(t, h, map[string]interface{}{"n": i})
	}

	h.resultCache.mu.Lock()
	size := len(h.resultCache.entries)
	h.resultCache.mu.Unlock()
	assert.LessOrEqual(t, size, maxResultCacheEntries+1)
}

func BenchmarkResultCache_Hit(b *testing.B) {
	mp, _ := newCountingCachePool("projects: a, b, c")
	h := NewHandler(mp, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetToolCacheTTL("cbm", "list_projects", time.Hour)
	args, _ := json.Marshal(map[string]interface{}{"server": "cbm", "tool": "list_projects"})
	req := &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")}
	params := ToolsCallParams{Name: "invoke_tool", Arguments: args}
	// Prime the cache.
	if _, err := h.handleLeanproxyTool(context.Background(), req, params); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.handleLeanproxyTool(context.Background(), req, params); err != nil {
			b.Fatal(err)
		}
	}
}
