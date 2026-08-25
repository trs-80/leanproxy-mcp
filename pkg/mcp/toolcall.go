package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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

	if resp := h.gateDispatch(req.ID, serverName, toolName); resp != nil {
		return resp, nil
	}

	// The truncation marker tells the model to adjust max_response_chars, so
	// this path must honor it too — and strip it before forwarding, unless
	// the upstream tool declares the parameter itself.
	var explicitCap int
	params.Arguments, explicitCap = h.extractResponseCap(serverName, toolName, params.Arguments)

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
	if capVal := h.responseCapFor(serverName, toolName, explicitCap); capVal > 0 {
		result = truncateToolResult(result, capVal, explicitCap > 0)
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

func (h *Handler) handleInvokeTool(ctx context.Context, req *Request, params ToolsCallParams) (*Response, error) {
	var serverName, toolName string
	var arguments json.RawMessage
	explicitCap := 0

	if params.Arguments != nil {
		// Decoded as raw messages: nested tool arguments must keep their
		// exact bytes (an interface{} round trip turns 64-bit IDs into
		// float64 and corrupts them).
		var args map[string]json.RawMessage
		if err := json.Unmarshal(params.Arguments, &args); err == nil {
			_ = json.Unmarshal(args["server"], &serverName)
			_ = json.Unmarshal(args["tool"], &toolName)
			if a, ok := args["arguments"]; ok {
				var probe map[string]json.RawMessage
				if json.Unmarshal(a, &probe) == nil {
					arguments = a
				}
			}
			if m, ok := args["max_response_chars"]; ok {
				explicitCap = parseCapValue(m)
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

	// Trim the stub prefix only when the raw name is not itself a known
	// upstream tool: real servers exist whose tool names literally start
	// with "<server>_" (brave_web_search on server brave), and trimming
	// those broke both the filter gate and dispatch.
	if strings.HasPrefix(toolName, serverName+"_") && h.lookupToolSchema(serverName, toolName) == nil {
		toolName = strings.TrimPrefix(toolName, serverName+"_")
	}

	if resp := h.gateDispatch(req.ID, serverName, toolName); resp != nil {
		return resp, nil
	}

	// A cap nested inside the tool arguments is a natural model slip (the
	// marker never says where the parameter goes); honor and strip it the
	// same way the direct path does. The top-level parameter wins.
	var nestedCap int
	arguments, nestedCap = h.extractResponseCap(serverName, toolName, arguments)
	if explicitCap == 0 {
		explicitCap = nestedCap
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
	if capVal := h.responseCapFor(serverName, toolName, explicitCap); capVal > 0 {
		result = truncateToolResult(result, capVal, explicitCap > 0)
	}

	return &Response{
		JSONRPC: JSONRPCVersion,
		Result:  result,
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
