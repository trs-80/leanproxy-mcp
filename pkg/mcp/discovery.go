package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/registry"
)

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
		now := time.Now()
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
				// Adaptive stubs: a tool unused for the configured window
				// renders name-only. It stays callable and searchable (the
				// full data lives in the tool cache), and one invocation
				// restores the full stub on the next tools/list.
				if h.stubDemoted(serverName, tool.Name, now) {
					gatewayTools = append(gatewayTools, Tool{
						Name:        stub.Name,
						InputSchema: json.RawMessage(`{"type":"object"}`),
					})
					continue
				}
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
		if h.usage != nil {
			h.usage.maybeSave(true)
		}

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

	// The cache-miss path reads the raw upstream tools/list, which the
	// include/exclude filter never touched — gate it like every other
	// dispatch surface or a denylisted tool's full schema stays
	// discoverable by guessing its name.
	if resp := h.gateDispatch(req.ID, serverName, toolName); resp != nil {
		return resp, nil
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

func enumChars(vals []interface{}) int {
	n := 0
	for _, v := range vals {
		n += len(fmt.Sprint(v))
	}
	return n
}

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
