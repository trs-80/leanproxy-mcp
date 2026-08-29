package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/toolstore"
)

// ToolCache is the in-memory per-server tool list, populated from the
// persistent store and refreshed from live servers.
type ToolCache struct {
	mu    sync.RWMutex
	tools map[string][]Tool
}

// storeTools writes a server's (filtered) tool list into the cache.
func (h *Handler) storeTools(serverName string, tools []Tool) {
	tools = h.filterTools(serverName, tools)
	h.toolCache.mu.Lock()
	h.toolCache.tools[serverName] = tools
	h.toolCache.mu.Unlock()
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
