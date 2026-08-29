// Package mcp implements the leanproxy gateway handler: a single MCP server
// that aggregates upstream MCP servers behind discovery (tools/list, list_tools,
// search_tools), dispatch (tools/call, invoke_tool, get_tool_schema), and the
// token-reduction levers (tool filters, result caps, lazy schema stubs).
//
// The Handler's method set is split by concern:
//
//   - dispatch.go: HandleRequest routing and the protocol-level methods
//   - toolcall.go: tools/call and invoke_tool execution paths
//   - discovery.go: tools/list stubs, list_tools, get_tool_schema
//   - search.go: search_tools ranking
//   - filters.go: include/exclude tool filters and the dispatch gate
//   - caps.go: max_response_chars resolution and result truncation
//   - toolcache.go: the in-memory and persistent tool caches
//   - format.go: signature formatting shared by discovery and errors
package mcp

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/pool"
	"github.com/mmornati/leanproxy-mcp/pkg/registry"
	"github.com/mmornati/leanproxy-mcp/pkg/toolstore"
)

// Handler is the gateway request handler. Configuration setters
// (SetToolFilter, SetTimeout, Set*MaxResponseChars, EnableLazyLoading) are not
// synchronized and must be called before the handler starts serving requests.
type Handler struct {
	pool            pool.ServerSource
	logger          *slog.Logger
	timeout         time.Duration
	timeouts        map[string]time.Duration
	toolCache       *ToolCache
	toolStore       toolstore.Cache
	cacheRefreshes  atomic.Uint64
	cacheFailures   atomic.Uint64
	lazyLoading     bool
	lazySchemaCache *registry.LazySchemaCache
	// defaultMaxResponseChars, when >0, caps every invoke_tool result that does
	// not carry an explicit max_response_chars argument.
	defaultMaxResponseChars int
	// toolFilters restricts which upstream tools a server contributes to the
	// cache (and hence to tools/list stubs, search_tools, list_tools).
	toolFilters map[string]toolFilter
	// toolResponseCaps holds per-tool result caps keyed "server/tool"; they
	// override defaultMaxResponseChars and are overridden by an explicit
	// max_response_chars argument.
	toolResponseCaps map[string]int
	// toolCacheTTLs holds per-tool exact-match result-cache TTLs keyed
	// "server/tool"; resultCache replays identical calls within the TTL.
	toolCacheTTLs map[string]time.Duration
	resultCache   *resultCache
	// minifyResults compacts JSON in result text blocks and drops duplicate
	// structuredContent (lossless; default on).
	minifyResults bool
	// adaptiveWindows enables usage-adaptive stubs per server: tools unused
	// for the window render name-only in lazy tools/list.
	adaptiveWindows map[string]time.Duration
	usage           *usageTracker
	// truncStats counts result-cap cuts per "server/tool"; truncMu guards it
	// (results are dispatched concurrently on the HTTP/SSE transports).
	truncMu    sync.Mutex
	truncStats map[string]TruncationStat
}

func NewHandler(p pool.ServerSource, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		pool:    p,
		logger:  logger,
		timeout: 30 * time.Second,
		toolCache: &ToolCache{
			tools: make(map[string][]Tool),
		},
		minifyResults: true,
	}
}

func NewHandlerWithToolStore(p pool.ServerSource, logger *slog.Logger, store toolstore.Cache) *Handler {
	h := NewHandler(p, logger)
	h.toolStore = store
	return h
}

// SetTimeout registers a per-server request timeout. The handler falls back
// to its default timeout (30s unless changed via SetDefaultTimeout) for any
// server that has no explicit entry. A zero duration clears the entry.
func (h *Handler) SetTimeout(serverName string, timeout time.Duration) {
	if h.timeouts == nil {
		h.timeouts = make(map[string]time.Duration)
	}
	if timeout <= 0 {
		delete(h.timeouts, serverName)
		return
	}
	h.timeouts[serverName] = timeout
}

// SetDefaultTimeout overrides the fallback timeout used when a server has no
// explicit per-server entry. A zero or negative value is ignored (the 30s
// default remains in effect).
func (h *Handler) SetDefaultTimeout(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	h.timeout = timeout
}

func (h *Handler) timeoutFor(serverName string) time.Duration {
	if d, ok := h.timeouts[serverName]; ok && d > 0 {
		return d
	}
	return h.timeout
}

func (h *Handler) EnableLazyLoading(ttl time.Duration) {
	h.lazyLoading = true
	h.lazySchemaCache = registry.NewLazySchemaCache(ttl)
	h.logger.Info("lazy loading enabled", "ttl", ttl)
}
