package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// toolFilter restricts which upstream tools a server exposes. A non-empty
// include set is an allowlist; exclude removes tools from whatever remains.
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

// filterTools applies the server's include/exclude lists via the same
// toolExposed predicate that gates dispatch, so discovery and dispatch can
// never diverge.
func (h *Handler) filterTools(serverName string, tools []Tool) []Tool {
	if _, ok := h.toolFilters[serverName]; !ok {
		return tools
	}
	kept := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if h.toolExposed(serverName, t.Name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// toolExposed reports whether the server's include/exclude filter leaves the
// tool callable. Filters gate dispatch as well as discovery: the docs present
// them as an allowlist/denylist, so a hidden tool must not remain invocable
// by guessing its name.
func (h *Handler) toolExposed(serverName, toolName string) bool {
	f, ok := h.toolFilters[serverName]
	if !ok {
		return true
	}
	if f.include != nil && !f.include[toolName] {
		return false
	}
	return !f.exclude[toolName]
}

// gateDispatch is the single policy choke point every dispatch surface
// (tools/call, invoke_tool, get_tool_schema, and any future executor) must
// pass. It returns a ready error response for a filtered-out tool, with
// close matches from the exposed set so a typo'd name on an include-list
// server reads as "did you mean", not as a policy block.
func (h *Handler) gateDispatch(reqID interface{}, serverName, toolName string) *Response {
	if h.toolExposed(serverName, toolName) {
		return nil
	}
	msg := fmt.Sprintf("tool %s is not exposed on server %s (tools filter in leanproxy config)", toolName, serverName)
	if s := h.suggestTools(serverName, toolName, 3); s != "" {
		msg += s
	}
	return &Response{
		JSONRPC: JSONRPCVersion,
		Error:   NewError(ErrCodeInvalidParams, msg),
		ID:      reqID,
	}
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
