package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/errors"
	"github.com/mmornati/leanproxy-mcp/pkg/pool"
	"github.com/mmornati/leanproxy-mcp/pkg/registry"
	"github.com/mmornati/leanproxy-mcp/pkg/toolstore"
)

type ParamInfo struct {
	Name        string
	Type        string
	IsRequired  bool
	Description string
}

type ToolCache struct {
	mu    sync.RWMutex
	tools map[string][]Tool
}

type Handler struct {
	pool            pool.ServerSource
	logger          *slog.Logger
	timeout         time.Duration
	timeouts        map[string]time.Duration
	toolCache       *ToolCache
	toolStore       toolstore.Cache
	manifest        *AggregatedManifest
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
}

type toolFilter struct {
	include map[string]bool
	exclude map[string]bool
}

// SetToolFilter restricts the tools exposed for a server. With a non-empty
// include list only those tools are kept; exclude removes tools from whatever
// remains. Under flat-rate billing every exposed tool is paid for on every
// turn (schema plus, on some clients, several name echoes), so trimming a
// server to the tools actually used is the largest per-turn lever.
func (h *Handler) SetToolFilter(serverName string, include, exclude []string) {
	if h.toolFilters == nil {
		h.toolFilters = make(map[string]toolFilter)
	}
	f := toolFilter{}
	if len(include) > 0 {
		f.include = make(map[string]bool, len(include))
		for _, n := range include {
			f.include[n] = true
		}
	}
	if len(exclude) > 0 {
		f.exclude = make(map[string]bool, len(exclude))
		for _, n := range exclude {
			f.exclude[n] = true
		}
	}
	h.toolFilters[serverName] = f
}

// SetToolMaxResponseChars sets a per-tool result cap (chars). Values below
// minResponseChars are raised to it; zero removes the cap.
func (h *Handler) SetToolMaxResponseChars(serverName, toolName string, n int) {
	if h.toolResponseCaps == nil {
		h.toolResponseCaps = make(map[string]int)
	}
	key := serverName + "/" + toolName
	if n <= 0 {
		delete(h.toolResponseCaps, key)
		return
	}
	if n < minResponseChars {
		n = minResponseChars
	}
	h.toolResponseCaps[key] = n
}

// responseCapFor resolves the effective result cap: explicit argument, then
// per-tool config, then the global default. Zero means unlimited.
func (h *Handler) responseCapFor(serverName, toolName string, explicit int) int {
	if explicit > 0 {
		if explicit < minResponseChars {
			return minResponseChars
		}
		return explicit
	}
	if n, ok := h.toolResponseCaps[serverName+"/"+toolName]; ok {
		return n
	}
	return h.defaultMaxResponseChars
}

// filterTools applies the server's include/exclude lists.
func (h *Handler) filterTools(serverName string, tools []Tool) []Tool {
	f, ok := h.toolFilters[serverName]
	if !ok || (f.include == nil && f.exclude == nil) {
		return tools
	}
	kept := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if f.include != nil && !f.include[t.Name] {
			continue
		}
		if f.exclude[t.Name] {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// storeTools writes a server's (filtered) tool list into the cache.
func (h *Handler) storeTools(serverName string, tools []Tool) {
	tools = h.filterTools(serverName, tools)
	h.toolCache.mu.Lock()
	h.toolCache.tools[serverName] = tools
	h.toolCache.mu.Unlock()
}

type AggregatedManifest struct {
	Tools     []Tool
	Resources []Resource
	Prompts   []Prompt
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
	}
}

func NewHandlerWithToolStore(p pool.ServerSource, logger *slog.Logger, store toolstore.Cache) *Handler {
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
		toolStore: store,
	}
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

func (h *Handler) HandleRequest(ctx context.Context, req *Request) (*Response, error) {
	h.logger.Debug("handling mcp request", "method", req.Method, "id", req.ID)

	if err := errors.ValidateContext(ctx); err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInternalError, err.Error()),
			ID:      req.ID,
		}, nil
	}

	switch req.Method {
	case MethodInitialize:
		return h.handleInitialize(ctx, req)
	case MethodInitialized:
		h.logger.Info("received initialized notification from client")
		return nil, nil
	case MethodResourcesList:
		return h.handleResourcesList(ctx, req)
	case MethodPromptsList:
		return h.handlePromptsList(ctx, req)
	case MethodToolsList:
		return h.handleToolsList(ctx, req)
	case "get_tool_schema":
		return h.handleGetToolSchema(ctx, req)
	case MethodToolsCall:
		return h.handleToolsCall(ctx, req)
	case MethodPing:
		return h.handlePing(ctx, req)
	case MethodShutdown:
		return h.handleShutdown(ctx, req)
	default:
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method)),
			ID:      req.ID,
		}, nil
	}
}

func (h *Handler) handleInitialize(ctx context.Context, req *Request) (*Response, error) {
	var params InitializeParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   NewError(ErrCodeInvalidParams, fmt.Sprintf("invalid params: %v", err)),
				ID:      req.ID,
			}, nil
		}
	}

	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: ServerCapabilities{
			Tools:     &ToolsCapability{ListChanged: false},
			Resources: &ResourcesCapability{ListChanged: false},
			Prompts:   &PromptsCapability{ListChanged: false},
		},
		ServerInfo: ServerInfo{
			Name:    "leanproxy-mcp",
			Version: "1.0.0",
		},
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInternalError, fmt.Sprintf("marshal result: %v", err)),
			ID:      req.ID,
		}, nil
	}

	h.logger.Info("initialized leanproxy-mcp", "client", params.ClientInfo.Name, "version", params.ClientInfo.Version)

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handleToolsList(ctx context.Context, req *Request) (*Response, error) {
	h.logger.Debug("tools/list request received, returning gateway tools only")

	gatewayTools := make([]Tool, 0)
	// In lazy mode every upstream tool is callable by name, so the gateway
	// wrappers are pure per-turn overhead (measured on a flat-rate client:
	// ~620 tokens/turn, never called). They stay for the non-lazy gateway.
	if !h.lazyLoading {
		for _, def := range GetAllToolDefinitions() {
			gatewayTools = append(gatewayTools, Tool{
				Name:        def.Name,
				Description: def.Description,
				InputSchema: def.InputSchema,
			})
		}
	}

	if h.lazyLoading {
		h.logger.Debug("lazy loading enabled, populating tool stubs from servers")
		h.toolCache.mu.RLock()
		empty := len(h.toolCache.tools) == 0
		h.toolCache.mu.RUnlock()
		if empty {
			// Go through the persistent store first: a slow upstream start
			// (codebase-memory was measured >10s) must not leave the client
			// with an empty tool list for the whole session.
			h.PopulateToolCache(ctx)
		}

		// Sorted output: identical caches must render identically across
		// sessions, otherwise map order shuffles the prefix and defeats
		// provider prompt caching.
		h.toolCache.mu.RLock()
		serverNames := make([]string, 0, len(h.toolCache.tools))
		for serverName := range h.toolCache.tools {
			serverNames = append(serverNames, serverName)
		}
		sort.Strings(serverNames)
		for _, serverName := range serverNames {
			tools := make([]Tool, len(h.toolCache.tools[serverName]))
			copy(tools, h.toolCache.tools[serverName])
			sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
			for _, tool := range tools {
				stub := registry.ToolStub{
					Name:        serverName + "_" + tool.Name,
					Description: truncateDescription(tool.Description, stubDescChars),
				}
				h.lazySchemaCache.SetFullSchema(stub.Name, registry.ToolSchema{
					Name:        tool.Name,
					Description: tool.Description,
					InputSchema: tool.InputSchema,
					ServerID:    serverName,
				})
				gatewayTools = append(gatewayTools, Tool{
					Name:        stub.Name,
					Description: stub.Description,
					// Compact schema: param names/types and the required list,
					// no prose. A bare {"type":"object"} was measured to send
					// the model arg-guessing (task2: 23 index_status calls to
					// reconstruct what one parameterized call returns), and a
					// schema without type "object" is rejected by the Anthropic
					// API, silently dropping the tool.
					InputSchema: compactSchema(tool.InputSchema),
				})
			}
		}
		h.toolCache.mu.RUnlock()

		h.logger.Info("lazy loading: sent tool stubs to client", "count", len(gatewayTools))
	}

	result := ToolsListResult{Tools: gatewayTools}
	resultBytes, _ := json.Marshal(result)

	h.logger.Info("gateway tools sent to client", "count", len(gatewayTools))

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) collectTools(ctx context.Context) (*AggregatedManifest, error) {
	return &AggregatedManifest{
		Tools:     make([]Tool, 0),
		Resources: make([]Resource, 0),
		Prompts:   make([]Prompt, 0),
	}, nil
}

func (h *Handler) initializeServer(ctx context.Context, serverName string) error {
	h.logger.Info("initializing server", "name", serverName)

	initParams := InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    ClientCapabilities{},
		ClientInfo: ClientInfo{
			Name:    "leanproxy-mcp",
			Version: "1.0.0",
		},
	}
	paramsBytes, _ := json.Marshal(initParams)

	h.logger.Debug("sending initialize request", "name", serverName, "params", string(paramsBytes))

	resp, err := h.pool.SendRequestToServerWithID(ctx, serverName, MethodInitialize, paramsBytes, 120*time.Second, 1)
	if err != nil {
		h.logger.Error("initialize request failed", "name", serverName, "error", err)
		return fmt.Errorf("initialize request failed: %w", err)
	}

	if resp != nil && resp.Error != nil {
		h.logger.Error("server returned initialize error", "name", serverName, "error", resp.Error.Message)
		return fmt.Errorf("server returned error: %s", resp.Error.Message)
	}

	h.logger.Debug("server initialized, sending initialized notification", "name", serverName)

	notifyErr := h.pool.SendServerNotification(ctx, serverName, "notifications/initialized", map[string]interface{}{
		"capabilities": ServerCapabilities{},
	})
	if notifyErr != nil {
		h.logger.Warn("failed to send initialized notification", "name", serverName, "error", notifyErr)
	}

	h.logger.Info("server ready", "name", serverName)
	return nil
}

func (h *Handler) handleToolsCall(ctx context.Context, req *Request) (*Response, error) {
	h.logger.Debug("handleToolsCall called", "params_len", len(req.Params))

	var params ToolsCallParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			h.logger.Warn("failed to unmarshal tools/call params", "error", err)
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   NewError(ErrCodeInvalidParams, fmt.Sprintf("invalid params: %v", err)),
				ID:      req.ID,
			}, nil
		}
	}

	h.logger.Debug("tools/call request", "name", params.Name)

	if params.Name == "" {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, "tool name is required"),
			ID:      req.ID,
		}, nil
	}

	if params.Name == "list_tools" || params.Name == "invoke_tool" || params.Name == "search_tools" {
		return h.handleLeanproxyTool(ctx, req, params)
	}

	serverName, toolName, err := h.parseToolName(params.Name)
	if err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, err.Error()),
			ID:      req.ID,
		}, nil
	}

	// Perform MCP initialize handshake if not yet done for this server instance.
	if !h.pool.IsServerMCPInitialized(serverName) {
		h.logger.Debug("initializing MCP session with server", "name", serverName)
		if err := h.initializeServer(ctx, serverName); err != nil {
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   NewError(ErrCodeServerError, fmt.Sprintf("server initialization failed: %v", err)),
				ID:      req.ID,
			}, nil
		}
		h.pool.MarkServerMCPInitialized(serverName)
	}

	newParams := ToolsCallParams{
		Name:      toolName,
		Arguments: params.Arguments,
	}
	paramsBytes, _ := json.Marshal(newParams)

	resp, err := h.pool.SendRequestToServer(ctx, serverName, MethodToolsCall, paramsBytes, h.timeoutFor(serverName))
	if err != nil {
		msg := fmt.Sprintf("tool call failed: %v", err)
		if suggestions := h.suggestTools(serverName, toolName, 3); suggestions != "" {
			msg += suggestions
		}
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeServerError, msg),
			ID:      req.ID,
		}, nil
	}

	result := resp.Result
	if cap := h.responseCapFor(serverName, toolName, 0); cap > 0 {
		result = truncateToolResult(result, cap)
	}

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  result,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handleLeanproxyTool(ctx context.Context, req *Request, params ToolsCallParams) (*Response, error) {
	switch params.Name {
	case "list_tools":
		return h.handleListTools(ctx, req, params)
	case "invoke_tool":
		return h.handleInvokeTool(ctx, req, params)
	case "search_tools":
		return h.handleSearchTools(ctx, req, params)
	default:
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeMethodNotFound, fmt.Sprintf("unknown gateway tool: %s", params.Name)),
			ID:      req.ID,
		}, nil
	}
}

func (h *Handler) handleListTools(ctx context.Context, req *Request, params ToolsCallParams) (*Response, error) {
	var serverName string
	var maxDescChars int
	if params.Arguments != nil {
		var args map[string]interface{}
		if err := json.Unmarshal(params.Arguments, &args); err == nil {
			args = ApplyDefaults("list_tools", args)
			if s, ok := args["server_name"].(string); ok {
				serverName = s
			}
			if m, ok := args["max_description_chars"].(float64); ok {
				maxDescChars = int(m)
			}
		}
	}

	if serverName == "" {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, "server_name parameter is required. Tip: search_tools finds tools across all servers in one call."),
			ID:      req.ID,
		}, nil
	}

	if valid, msg := ValidateParam("list_tools", "max_description_chars", float64(maxDescChars)); !valid {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, fmt.Sprintf("max_description_chars %s", msg)),
			ID:      req.ID,
		}, nil
	}

	if maxDescChars == 0 {
		maxDescChars = 200
	}

	h.logger.Info("list_tools called", "server_name", serverName, "max_desc_chars", maxDescChars)

	servers := h.pool.ListServers()
	serverFound := false
	for _, s := range servers {
		if s == serverName {
			serverFound = true
			break
		}
	}

	if !serverFound {
		serversList := strings.Join(servers, ", ")
		result := map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": fmt.Sprintf("Server '%s' not found. Available servers: %s.", serverName, serversList)},
			},
		}
		resultBytes, _ := json.Marshal(result)
		return &Response{
			JSONRPC: JSONRPCVersion,
			Result:  resultBytes,
			ID:      req.ID,
		}, nil
	}

	h.toolCache.mu.RLock()
	tools, exists := h.toolCache.tools[serverName]
	h.toolCache.mu.RUnlock()

	if !exists || len(tools) == 0 {
		h.PopulateToolCache(ctx)
		h.toolCache.mu.RLock()
		tools = h.toolCache.tools[serverName]
		h.toolCache.mu.RUnlock()
	}

	if len(tools) == 0 {
		result := map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": fmt.Sprintf("No tools available on server '%s'. The server may be unavailable or have no tools.", serverName)},
			},
		}
		resultBytes, _ := json.Marshal(result)
		return &Response{
			JSONRPC: JSONRPCVersion,
			Result:  resultBytes,
			ID:      req.ID,
		}, nil
	}

	formattedTools := make([]string, 0, len(tools))
	for _, tool := range tools {
		formatted := formatTool(tool, serverName, maxDescChars)
		formattedTools = append(formattedTools, formatted)
	}

	h.logger.Info("list_tools completed", "server", serverName, "results", len(formattedTools))

	result := map[string]interface{}{
		"content": []map[string]string{
			{"type": "text", "text": fmt.Sprintf("%s tools (%d):\n%s", serverName, len(tools), strings.Join(formattedTools, "\n"))},
		},
	}
	resultBytes, _ := json.Marshal(result)

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) PopulateToolCache(ctx context.Context) {
	h.logger.Info("populating tool cache from backend servers")

	if h.toolStore != nil {
		h.loadFromPersistentCache(ctx)
	}

	h.refreshToolCacheFromServers(ctx)

	h.logger.Info("tool cache population complete")
}

func (h *Handler) loadFromPersistentCache(ctx context.Context) {
	servers := h.pool.ListServers()
	for _, serverName := range servers {
		cachedTools, err := h.toolStore.GetTools(serverName)
		if err != nil {
			h.logger.Warn("failed to load tools from persistent cache", "server", serverName, "error", err)
			continue
		}
		if cachedTools == nil {
			continue
		}

		tools := make([]Tool, len(cachedTools))
		for i, ct := range cachedTools {
			tools[i] = Tool{
				Name:        ct.Name,
				Description: ct.Description,
				InputSchema: ct.InputSchema,
			}
		}

		h.storeTools(serverName, tools)

		h.logger.Debug("loaded tools from persistent cache", "server", serverName, "count", len(tools))
	}
}

func (h *Handler) refreshToolCacheFromServers(ctx context.Context) {
	h.cacheRefreshes.Add(1)
	servers := h.pool.ListServers()

	if len(servers) == 0 {
		h.logger.Debug("no servers to refresh")
		return
	}

	type serverToolResult struct {
		name      string
		tools     []Tool
		err       error
		initErr   error
		respError string
		hasResult bool
	}

	var wg sync.WaitGroup
	results := make(chan serverToolResult, len(servers))

	for _, serverName := range servers {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				results <- serverToolResult{name: name, err: ctx.Err()}
				return
			default:
			}

			serverCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			h.logger.Debug("checking server for cache refresh", "name", name)

			state, _ := h.pool.GetServerState(name)
			h.logger.Debug("server state", "name", name, "state", state)

			if state != "idle" && state != "running" && state != "busy" {
				h.logger.Warn("server not running, attempting restart for cache refresh", "name", name, "state", state)

				var restartErr error
				for attempt := 0; attempt < 3; attempt++ {
					if attempt > 0 {
						h.logger.Info("retrying server restart for cache", "name", name, "attempt", attempt+1)
						time.Sleep(time.Duration(attempt) * time.Second)
					}

					if err := h.pool.RestartServer(serverCtx, name); err != nil {
						restartErr = err
						continue
					}
					restartErr = nil
					break
				}

				if restartErr != nil {
					h.logger.Error("failed to restart server for cache after retries", "name", name, "error", restartErr)
					h.cacheFailures.Add(1)
					results <- serverToolResult{name: name, err: restartErr}
					return
				}
			}

			initErr := h.initializeServer(serverCtx, name)
			if initErr != nil {
				h.logger.Warn("failed to initialize server, will try without initialization", "name", name, "error", initErr)
			}

			h.logger.Debug("requesting tools/list for cache", "name", name)
			resp, err := h.pool.SendRequestToServer(serverCtx, name, MethodToolsList, nil, 10*time.Second)
			if err != nil {
				h.logger.Error("failed to get tools for cache", "name", name, "error", err)
				h.cacheFailures.Add(1)
				results <- serverToolResult{name: name, err: err, initErr: initErr}
				return
			}

			if resp != nil && resp.Error != nil {
				h.logger.Error("server error during cache population", "name", name, "error", resp.Error.Message)
				h.cacheFailures.Add(1)
				results <- serverToolResult{name: name, respError: resp.Error.Message, initErr: initErr}
				return
			}

			if resp == nil || resp.Result == nil {
				h.logger.Error("server returned no result for cache", "name", name, "resp", fmt.Sprintf("%+v", resp))
				h.cacheFailures.Add(1)
				results <- serverToolResult{name: name, initErr: initErr}
				return
			}

			if len(resp.Result) == 0 || string(resp.Result) == "null" {
				h.logger.Error("server returned null/empty result for cache", "name", name, "resp", fmt.Sprintf("%+v", resp))
				h.cacheFailures.Add(1)
				results <- serverToolResult{name: name, initErr: initErr}
				return
			}

			var toolsResult ToolsListResult
			if err := json.Unmarshal(resp.Result, &toolsResult); err != nil {
				h.logger.Error("failed to parse tools for cache", "name", name, "error", err, "result", string(resp.Result))
				h.cacheFailures.Add(1)
				results <- serverToolResult{name: name, err: err, initErr: initErr}
				return
			}

			h.logger.Debug("caching tools from server", "name", name, "count", len(toolsResult.Tools))
			results <- serverToolResult{name: name, tools: toolsResult.Tools, hasResult: true, initErr: initErr}
		}(serverName)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		if !result.hasResult {
			continue
		}

		h.storeTools(result.name, result.tools)

		if h.toolStore != nil {
			if err := h.toolStore.SetTools(result.name, toolsToCachedTools(result.tools)); err != nil {
				h.logger.Warn("failed to persist tools to cache", "name", result.name, "error", err)
			}
		}

		h.logger.Debug("cached tools from server", "name", result.name, "count", len(result.tools))
	}
}

// handleSearchTools answers the search_tools gateway tool: a single-call,
// cross-server keyword search that returns invocation-ready signatures so the
// client can call invoke_tool without any further discovery round trips.
func (h *Handler) handleSearchTools(ctx context.Context, req *Request, params ToolsCallParams) (*Response, error) {
	var query, serverFilter string
	limit := 25
	maxDescChars := 120
	if params.Arguments != nil {
		var args map[string]interface{}
		if err := json.Unmarshal(params.Arguments, &args); err == nil {
			if q, ok := args["query"].(string); ok {
				query = q
			}
			if sv, ok := args["server"].(string); ok {
				serverFilter = sv
			}
			if l, ok := args["limit"].(float64); ok && l > 0 {
				limit = int(l)
			}
			if m, ok := args["max_description_chars"].(float64); ok && m > 0 {
				maxDescChars = int(m)
			}
		}
	}

	h.toolCache.mu.RLock()
	empty := len(h.toolCache.tools) == 0
	h.toolCache.mu.RUnlock()
	if empty {
		h.PopulateToolCache(ctx)
	}

	matches := h.searchToolCacheFiltered(query, serverFilter, maxDescChars)
	total := len(matches)
	truncated := false
	if total > limit {
		matches = matches[:limit]
		truncated = true
	}

	var text string
	switch {
	case total == 0 && serverFilter != "":
		text = fmt.Sprintf("No tools matching %q on server %q. Try a broader query or drop the server filter.", query, serverFilter)
	case total == 0:
		text = fmt.Sprintf("No tools matching %q. Try fewer or more general keywords.", query)
	default:
		header := fmt.Sprintf("%d tools ([required] {optional}); call invoke_tool with server, tool, arguments:\n", total)
		text = header + strings.Join(matches, "\n")
		if truncated {
			text += fmt.Sprintf("\n... %d more; narrow the query or raise limit.", total-limit)
		}
	}

	result := map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
	resultBytes, _ := json.Marshal(result)
	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

// searchToolCacheFiltered ranks tools against the query with scored-OR
// matching: any query word may hit (name hits outrank description hits, all
// words matching outranks partial), so "trace path callers" still surfaces
// trace_path even though "callers" appears nowhere. All-words AND matching was
// measured to strand real sessions: the model searched 2-3 times, got nothing,
// and fell back to a full list_tools — costing more than no search at all.
// Output is deterministic: score desc, then server/tool name.
func (h *Handler) searchToolCacheFiltered(query, serverFilter string, maxDescChars int) []string {
	h.toolCache.mu.RLock()
	defer h.toolCache.mu.RUnlock()

	queryWords := strings.Fields(strings.ToLower(query))

	type scored struct {
		server string
		tool   Tool
		score  int
	}
	var matches []scored

	serverNames := make([]string, 0, len(h.toolCache.tools))
	for serverName := range h.toolCache.tools {
		if serverFilter != "" && serverName != serverFilter {
			continue
		}
		serverNames = append(serverNames, serverName)
	}
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		tools := h.toolCache.tools[serverName]
		sorted := make([]Tool, len(tools))
		copy(sorted, tools)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, tool := range sorted {
			if len(queryWords) == 0 {
				matches = append(matches, scored{serverName, tool, 0})
				continue
			}
			name := strings.ToLower(serverName + "_" + tool.Name)
			desc := strings.ToLower(tool.Description)
			score, hits := 0, 0
			for _, word := range queryWords {
				switch {
				case strings.Contains(name, word):
					score += 10
					hits++
				case strings.Contains(desc, word):
					score += 3
					hits++
				}
			}
			if hits == 0 {
				continue
			}
			if hits == len(queryWords) {
				score += fullCoverageBonus
			}
			matches = append(matches, scored{serverName, tool, score})
		}
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })

	// Precision guard: when enough tools match every query word, weaker
	// partial matches are noise that inflates the payload (the whole point of
	// search over list_tools). Partial matches serve only as a fallback so a
	// near-miss query still returns something actionable instead of nothing.
	full := 0
	for _, m := range matches {
		if m.score >= fullCoverageBonus {
			full++
		}
	}
	if full >= minFullMatchesForPrecision {
		matches = matches[:full]
	}

	results := make([]string, 0, len(matches))
	for _, m := range matches {
		required, optional := parseInputSchema(m.tool.InputSchema)
		results = append(results, formatToolSearchResult(m.server, m.tool.Name, m.tool.Description, required, optional, maxDescChars))
	}
	return results
}

func (h *Handler) searchToolCache(query string, maxDescChars int) []string {
	h.toolCache.mu.RLock()
	defer h.toolCache.mu.RUnlock()

	var results []string
	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)

	for serverName, tools := range h.toolCache.tools {
		for _, tool := range tools {
			matchedLine := fmt.Sprintf("%s_%s: %s", serverName, tool.Name, strings.ToLower(truncateDescription(tool.Description, maxDescChars)))
			if query == "" || matchesQuery(matchedLine, queryWords) {
				required, optional := parseInputSchema(tool.InputSchema)
				formatted := formatToolSearchResult(serverName, tool.Name, tool.Description, required, optional, maxDescChars)
				results = append(results, formatted)
			}
		}
	}

	return results
}

func matchesQuery(text string, queryWords []string) bool {
	for _, word := range queryWords {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

func (h *Handler) handleInvokeTool(ctx context.Context, req *Request, params ToolsCallParams) (*Response, error) {
	var serverName, toolName string
	var arguments json.RawMessage
	var err error
	explicitCap := 0

	if params.Arguments != nil {
		var args map[string]interface{}
		if err := json.Unmarshal(params.Arguments, &args); err == nil {
			args = ApplyDefaults("invoke_tool", args)
			if s, ok := args["server"].(string); ok {
				serverName = s
			}
			if t, ok := args["tool"].(string); ok {
				toolName = t
			}
			if a, ok := args["arguments"].(map[string]interface{}); ok {
				arguments, err = json.Marshal(a)
				if err != nil {
					h.logger.Warn("failed to marshal arguments", "error", err)
				}
			}
			if m, ok := args["max_response_chars"].(float64); ok && m > 0 {
				explicitCap = int(m)
			}
		}
	}

	if serverName == "" || toolName == "" {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, "server and tool are required. Tip: search_tools returns server, tool, and parameters in one call."),
			ID:      req.ID,
		}, nil
	}

	if strings.HasPrefix(toolName, serverName+"_") {
		toolName = strings.TrimPrefix(toolName, serverName+"_")
	}

	h.logger.Info("invoke_tool called", "server", serverName, "tool", toolName)

	state, stateErr := h.pool.GetServerState(serverName)
	h.logger.Debug("server current state", "name", serverName, "state", state, "error", stateErr)

	if state != "idle" && state != "running" && state != "busy" {
		h.logger.Warn("server not running, attempting to restart", "name", serverName, "state", state)

		var restartErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				h.logger.Info("retrying server restart", "name", serverName, "attempt", attempt+1)
				time.Sleep(time.Duration(attempt) * time.Second)
			}

			if err := h.pool.RestartServer(ctx, serverName); err != nil {
				restartErr = err
				continue
			}
			restartErr = nil
			break
		}

		if restartErr != nil {
			h.logger.Error("failed to restart server after retries", "name", serverName, "error", restartErr)
			enrichedError := FormatErrorWithHint(
				fmt.Sprintf("server %s is not running (state: %s) and failed to restart after retries: %v", serverName, state, restartErr),
				serverName, toolName,
			)
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   NewError(ErrCodeServerError, enrichedError),
				ID:      req.ID,
			}, nil
		}
		h.logger.Info("server restarted successfully", "name", serverName)
	}

	// Perform MCP initialize handshake if not yet done for this server instance.
	// The MCP protocol requires initialize + notifications/initialized before any tool call.
	if !h.pool.IsServerMCPInitialized(serverName) {
		h.logger.Debug("initializing MCP session with server", "name", serverName)
		if err := h.initializeServer(ctx, serverName); err != nil {
			h.logger.Error("invoke_tool: server initialization failed", "server", serverName, "error", err)
			schema := h.lookupToolSchema(serverName, toolName)
			enrichedError := FormatErrorWithHint(fmt.Sprintf("server initialization failed: %v", err), serverName, toolName)
			errResp := NewError(ErrCodeServerError, enrichedError)
			if schema != nil {
				dataBytes, _ := json.Marshal(map[string]interface{}{
					"tool":   toolName,
					"schema": json.RawMessage(schema),
				})
				errResp.Data = dataBytes
			}
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   errResp,
				ID:      req.ID,
			}, nil
		}
		h.pool.MarkServerMCPInitialized(serverName)
	}

	newParams := ToolsCallParams{
		Name:      toolName,
		Arguments: arguments,
	}
	paramsBytes, _ := json.Marshal(newParams)

	resp, err := h.pool.SendRequestToServer(ctx, serverName, MethodToolsCall, paramsBytes, h.timeoutFor(serverName))
	if err != nil {
		h.logger.Error("invoke_tool failed", "server", serverName, "tool", toolName, "error", err)
		schema := h.lookupToolSchema(serverName, toolName)
		enrichedError := FormatErrorWithHint(fmt.Sprintf("tool invocation failed: %v", err), serverName, toolName)
		if schema == nil {
			if suggestions := h.suggestTools(serverName, toolName, 3); suggestions != "" {
				enrichedError += suggestions
			}
		}
		errResp := NewError(ErrCodeServerError, enrichedError)
		if schema != nil {
			dataBytes, _ := json.Marshal(map[string]interface{}{
				"tool":   toolName,
				"schema": json.RawMessage(schema),
			})
			errResp.Data = dataBytes
		}
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   errResp,
			ID:      req.ID,
		}, nil
	}

	result := resp.Result
	if cap := h.responseCapFor(serverName, toolName, explicitCap); cap > 0 {
		result = truncateToolResult(result, cap)
	}

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  result,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handleGetToolSchema(ctx context.Context, req *Request) (*Response, error) {
	if !h.lazyLoading || h.lazySchemaCache == nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeMethodNotFound, "lazy loading not enabled"),
			ID:      req.ID,
		}, nil
	}

	var params struct {
		Name string `json:"name"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &Response{
				JSONRPC: JSONRPCVersion,
				Error:   NewError(ErrCodeInvalidParams, fmt.Sprintf("invalid params: %v", err)),
				ID:      req.ID,
			}, nil
		}
	}

	if params.Name == "" {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, "tool name is required"),
			ID:      req.ID,
		}, nil
	}

	schema, found := h.lazySchemaCache.GetFullSchema(params.Name)
	if found {
		h.logger.Debug("lazy loading: cache hit", "tool", params.Name)
		resultBytes, _ := json.Marshal(schema)
		return &Response{
			JSONRPC: JSONRPCVersion,
			Result:  resultBytes,
			ID:      req.ID,
		}, nil
	}

	h.logger.Debug("lazy loading: cache miss, fetching from MCP server", "tool", params.Name)

	serverName, toolName, err := h.parseToolName(params.Name)
	if err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, err.Error()),
			ID:      req.ID,
		}, nil
	}

	if err := h.initializeServer(ctx, serverName); err != nil {
		h.logger.Warn("lazy loading: failed to initialize server, continuing anyway", "server", serverName, "error", err)
	}

	resp, err := h.pool.SendRequestToServer(ctx, serverName, MethodToolsList, nil, 10*time.Second)
	if err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeServerError, fmt.Sprintf("failed to fetch tool schema: %v", err)),
			ID:      req.ID,
		}, nil
	}

	if resp.Error != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeServerError, resp.Error.Message),
			ID:      req.ID,
		}, nil
	}

	var toolsResult ToolsListResult
	if err := json.Unmarshal(resp.Result, &toolsResult); err != nil {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInternalError, fmt.Sprintf("failed to parse tool schema: %v", err)),
			ID:      req.ID,
		}, nil
	}

	var fullSchema registry.ToolSchema
	for _, tool := range toolsResult.Tools {
		if tool.Name == toolName {
			fullSchema = registry.ToolSchema{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
				ServerID:    serverName,
			}
			break
		}
	}

	if fullSchema.Name == "" {
		return &Response{
			JSONRPC: JSONRPCVersion,
			Error:   NewError(ErrCodeInvalidParams, fmt.Sprintf("tool not found: %s", params.Name)),
			ID:      req.ID,
		}, nil
	}

	h.lazySchemaCache.SetFullSchema(params.Name, fullSchema)

	h.logger.Info("lazy loading: schema loaded and cached", "tool", params.Name)

	resultBytes, _ := json.Marshal(fullSchema)
	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) parseToolName(fullName string) (serverName, toolName string, err error) {
	parts := strings.SplitN(fullName, "_", 2)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid tool name '%s': expected format is 'serverName_toolName'", fullName)
	}
	return parts[0], parts[1], nil
}

func (h *Handler) handleResourcesList(ctx context.Context, req *Request) (*Response, error) {
	result := ResourcesListResult{
		Resources: make([]Resource, 0),
	}
	resultBytes, _ := json.Marshal(result)

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handlePromptsList(ctx context.Context, req *Request) (*Response, error) {
	result := PromptsListResult{
		Prompts: make([]Prompt, 0),
	}
	resultBytes, _ := json.Marshal(result)

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handlePing(ctx context.Context, req *Request) (*Response, error) {
	result := map[string]string{"status": "ok"}
	resultBytes, _ := json.Marshal(result)

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) handleShutdown(ctx context.Context, req *Request) (*Response, error) {
	result := map[string]string{"status": "shutdown"}
	resultBytes, _ := json.Marshal(result)

	h.pool.Close()

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  resultBytes,
		ID:      req.ID,
	}, nil
}

func (h *Handler) ResetManifest() {
	h.manifest = nil
}

func parseInputSchema(schema json.RawMessage) (required, optional []ParamInfo) {
	var schemaMap map[string]interface{}
	if err := json.Unmarshal(schema, &schemaMap); err != nil {
		return nil, nil
	}

	properties, ok := schemaMap["properties"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	var requiredNames []string
	if req, ok := schemaMap["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				requiredNames = append(requiredNames, s)
			}
		}
	}

	isRequired := make(map[string]bool)
	for _, name := range requiredNames {
		isRequired[name] = true
	}

	for name, prop := range properties {
		propMap, ok := prop.(map[string]interface{})
		if !ok {
			continue
		}
		typeVal, _ := propMap["type"].(string)
		descVal, _ := propMap["description"].(string)

		param := ParamInfo{
			Name:        name,
			Type:        typeVal,
			IsRequired:  isRequired[name],
			Description: descVal,
		}

		if isRequired[name] {
			required = append(required, param)
		} else {
			optional = append(optional, param)
		}
	}

	// Deterministic order: identical schemas must always render identically —
	// unstable output defeats provider prompt caching across sessions and makes
	// results harder to diff. Required params follow the schema's "required"
	// array (author-intended order); optional params sort alphabetically.
	requiredRank := make(map[string]int, len(requiredNames))
	for i, name := range requiredNames {
		requiredRank[name] = i
	}
	sort.Slice(required, func(i, j int) bool { return requiredRank[required[i].Name] < requiredRank[required[j].Name] })
	sort.Slice(optional, func(i, j int) bool { return optional[i].Name < optional[j].Name })
	return required, optional
}

func formatToolSearchResult(serverName, toolName, description string, required, optional []ParamInfo, maxDescChars int) string {
	var sb strings.Builder
	sb.WriteString(serverName)
	sb.WriteString("_")
	sb.WriteString(toolName)
	sb.WriteString(": ")
	sb.WriteString(truncateDescription(description, maxDescChars))

	if len(required) > 0 {
		sb.WriteString(" [")
		for i, p := range required {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(p.Name)
			sb.WriteString(": ")
			sb.WriteString(p.Type)
		}
		sb.WriteString("]")
	}

	if len(optional) > 0 {
		sb.WriteString(" {")
		for i, p := range optional {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(p.Name)
			sb.WriteString(": ")
			sb.WriteString(p.Type)
		}
		sb.WriteString("}")
	}

	return sb.String()
}

func formatTool(tool Tool, serverName string, maxDescChars int) string {
	required, optional := parseInputSchema(tool.InputSchema)
	return formatToolSearchResult(serverName, tool.Name, tool.Description, required, optional, maxDescChars)
}

// truncateDescription cuts at a word boundary and appends a single ellipsis
// rune: a mid-word cut wastes the partial token, and "…" tokenizes shorter
// than "...". Falls back to a hard cut when no space is found in the last
// third of the budget.
func truncateDescription(description string, maxChars int) string {
	if maxChars <= 0 || len(description) <= maxChars {
		return description
	}
	const ellipsis = "…" // 3 bytes
	if maxChars <= len(ellipsis) {
		return description[:maxChars]
	}
	cut := maxChars - len(ellipsis)
	if i := strings.LastIndex(description[:cut], " "); i >= cut*2/3 {
		cut = i
	}
	return strings.TrimRight(description[:cut], " ,;:-") + ellipsis
}

func (h *Handler) lookupToolSchema(serverName, toolName string) json.RawMessage {
	h.toolCache.mu.RLock()
	defer h.toolCache.mu.RUnlock()

	tools, ok := h.toolCache.tools[serverName]
	if !ok {
		return nil
	}

	for _, tool := range tools {
		if tool.Name == toolName {
			return tool.InputSchema
		}
	}
	return nil
}

func toolsToCachedTools(tools []Tool) []toolstore.CachedTool {
	result := make([]toolstore.CachedTool, len(tools))
	for i, t := range tools {
		result[i] = toolstore.CachedTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return result
}

// minResponseChars is the smallest cap accepted for max_response_chars; below
// this a truncated result is unlikely to be usable and the marker overhead
// dominates.
const minResponseChars = 200

// truncationMarkerSlack: results over the cap by less than this are passed
// through untouched, since the truncation marker is itself ~90 chars.
const truncationMarkerSlack = 100

// SetDefaultMaxResponseChars sets a server-side default cap applied to every
// invoke_tool result that does not carry an explicit max_response_chars.
// Zero (the default) means unlimited.
func (h *Handler) SetDefaultMaxResponseChars(n int) {
	if n > 0 && n < minResponseChars {
		n = minResponseChars
	}
	h.defaultMaxResponseChars = n
}

// truncateToolResult enforces a total character budget across the text blocks
// of an MCP tools/call result. Non-text blocks and unparseable results pass
// through untouched (never corrupt what we do not understand). A marker noting
// the cut is appended so the model knows the output is partial and how to get
// the rest.
func truncateToolResult(raw json.RawMessage, maxChars int) json.RawMessage {
	var result struct {
		Content []map[string]interface{} `json:"content"`
		IsError *bool                    `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Content) == 0 {
		return raw
	}

	total := 0
	for _, block := range result.Content {
		if text, ok := block["text"].(string); ok {
			total += len(text)
		}
	}
	// Truncating only pays when it saves more than the marker costs.
	if total <= maxChars+truncationMarkerSlack {
		return raw
	}

	budget := maxChars
	kept := make([]map[string]interface{}, 0, len(result.Content))
	for _, block := range result.Content {
		text, ok := block["text"].(string)
		if !ok {
			kept = append(kept, block)
			continue
		}
		if budget <= 0 {
			break
		}
		if len(text) > budget {
			nb := make(map[string]interface{}, len(block))
			for k, v := range block {
				nb[k] = v
			}
			nb["text"] = text[:budget]
			kept = append(kept, nb)
			budget = 0
			break
		}
		budget -= len(text)
		kept = append(kept, block)
	}

	marker := fmt.Sprintf("\n[leanproxy: truncated, %d of %d chars shown; raise or omit max_response_chars for more]", maxChars, total)
	kept = append(kept, map[string]interface{}{"type": "text", "text": marker})

	out := map[string]interface{}{"content": kept}
	if result.IsError != nil {
		out["isError"] = *result.IsError
	}
	trimmed, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return trimmed
}

// suggestTools returns a "did you mean" block for an unknown tool: up to max
// close matches on the same server (fallback: any server), formatted with
// parameter signatures so the model can retry immediately without a discovery
// round trip.
func (h *Handler) suggestTools(serverName, toolName string, max int) string {
	needle := strings.ToLower(toolName)

	h.toolCache.mu.RLock()
	defer h.toolCache.mu.RUnlock()

	type cand struct {
		server string
		tool   Tool
		score  int
	}
	var cands []cand
	consider := func(server string, tools []Tool, bonus int) {
		for _, t := range tools {
			name := strings.ToLower(t.Name)
			score := 0
			switch {
			case name == needle:
				score = 100
			case strings.Contains(name, needle) || strings.Contains(needle, name):
				score = 60
			default:
				common := 0
				for i := 0; i < len(name) && i < len(needle) && name[i] == needle[i]; i++ {
					common++
				}
				if common >= 4 {
					score = common
				}
				for _, word := range strings.FieldsFunc(needle, func(r rune) bool { return r == '_' || r == '-' }) {
					if len(word) >= 4 && strings.Contains(name, word) {
						score += 20
					}
				}
			}
			if score > 0 {
				cands = append(cands, cand{server, t, score + bonus})
			}
		}
	}

	if tools, ok := h.toolCache.tools[serverName]; ok {
		consider(serverName, tools, 10)
	}
	if len(cands) == 0 {
		for server, tools := range h.toolCache.tools {
			if server == serverName {
				continue
			}
			consider(server, tools, 0)
		}
	}
	if len(cands) == 0 {
		return ""
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].server != cands[j].server {
			return cands[i].server < cands[j].server
		}
		return cands[i].tool.Name < cands[j].tool.Name
	})
	if len(cands) > max {
		cands = cands[:max]
	}

	var sb strings.Builder
	sb.WriteString("\nClose matches ([required] {optional}):\n")
	for i, c := range cands {
		if i > 0 {
			sb.WriteString("\n")
		}
		required, optional := parseInputSchema(c.tool.InputSchema)
		sb.WriteString(formatToolSearchResult(c.server, c.tool.Name, c.tool.Description, required, optional, 80))
	}
	return sb.String()
}

// stubDescChars caps per-tool description length in lazy-loading tools/list
// stubs. Stubs exist for name discovery; the full description travels with
// get_tool_schema, so every char here is paid in every conversation for
// marginal value.
const stubDescChars = 160

// stubParamDescChars caps per-parameter description length inside compact stub
// schemas.
const stubParamDescChars = 48

// stubEnumMaxValues / stubEnumMaxChars bound which enums survive compaction.
const (
	stubEnumMaxValues = 6
	stubEnumMaxChars  = 60
)

func enumChars(vals []interface{}) int {
	n := 0
	for _, v := range vals {
		n += len(fmt.Sprint(v))
	}
	return n
}

// fullCoverageBonus is added when every query word matches, guaranteeing
// full-coverage matches sort above any partial match (max per-word score is
// 10, so partials cannot reach it without full coverage).
const fullCoverageBonus = 1000

// minFullMatchesForPrecision: with at least this many full-coverage matches,
// partial matches are dropped from search results entirely.
const minFullMatchesForPrecision = 1

// compactSchema reduces a JSON-schema to parameter names, types, and the
// required list — the minimum a model needs to call the tool correctly without
// guessing. Per-param descriptions, enums, defaults, and nested detail are
// dropped; they account for most of a schema's bytes and are recoverable via
// get_tool_schema or search_tools. Falls back to a valid empty object schema
// when the input cannot be parsed.
func compactSchema(schema json.RawMessage) json.RawMessage {
	fallback := json.RawMessage(`{"type":"object"}`)
	var full struct {
		Properties map[string]struct {
			Type        string        `json:"type"`
			Description string        `json:"description"`
			Enum        []interface{} `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &full); err != nil || len(full.Properties) == 0 {
		return fallback
	}
	props := make(map[string]map[string]interface{}, len(full.Properties))
	for name, p := range full.Properties {
		t := p.Type
		if t == "" {
			t = "string"
		}
		props[name] = map[string]interface{}{"type": t}
		// A short description per param stays: without it models guess which
		// params exist for what (measured on Bob: a bare include_details flag
		// went unused and the model fanned out 23 per-item calls instead of 1).
		if d := truncateDescription(p.Description, stubParamDescChars); d != "" {
			props[name]["description"] = d
		}
		// Short enums are cheap insurance: ~30 bytes per turn versus a whole
		// re-billed turn when the model guesses an invalid value.
		if len(p.Enum) > 0 && len(p.Enum) <= stubEnumMaxValues && enumChars(p.Enum) <= stubEnumMaxChars {
			props[name]["enum"] = p.Enum
		}
	}
	out := map[string]interface{}{"type": "object", "properties": props}
	if len(full.Required) > 0 {
		sort.Strings(full.Required)
		out["required"] = full.Required
	}
	b, err := json.Marshal(out)
	if err != nil {
		return fallback
	}
	return b
}
