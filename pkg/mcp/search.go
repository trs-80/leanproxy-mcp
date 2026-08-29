package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// fullCoverageBonus is added when every query word matches, guaranteeing
// full-coverage matches sort above any partial match (max per-word score is
// 10, so partials cannot reach it without full coverage).
const fullCoverageBonus = 1000

// nearMissMinQueryWords: the query length at which a partial match that misses
// exactly one word is kept alongside full-coverage matches.
//
// Dropping every partial the moment ONE tool covered all the words measured as
// a real discovery failure: "search graph trace callers" returned search_graph
// alone (4/4) and hid trace_path (3/4, missing only "search"), so the model
// never learned the tool it was asking for existed and spent four turns
// without it. But on a short query the same "misses one word" is genuine
// noise — for "create issue", create_branch matches only half the query.
// Three words is where missing one still means most of the query matched.
const nearMissMinQueryWords = 3

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
// Output is deterministic: score desc, then server name, then tool name.
func (h *Handler) searchToolCacheFiltered(query, serverFilter string, maxDescChars int) []string {
	h.toolCache.mu.RLock()
	defer h.toolCache.mu.RUnlock()

	queryWords := strings.Fields(strings.ToLower(query))

	type scored struct {
		server string
		tool   Tool
		score  int
		hits   int
	}
	var matches []scored

	for serverName, tools := range h.toolCache.tools {
		if serverFilter != "" && serverName != serverFilter {
			continue
		}
		for _, tool := range tools {
			if len(queryWords) == 0 {
				matches = append(matches, scored{serverName, tool, 0, 0})
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
			matches = append(matches, scored{serverName, tool, score, hits})
		}
	}

	// One deterministic sort over the match set replaces the previous
	// per-server copy+sort of every tool list on every query: score desc,
	// then server/tool name for a stable render across identical caches.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].server != matches[j].server {
			return matches[i].server < matches[j].server
		}
		return matches[i].tool.Name < matches[j].tool.Name
	})

	// Precision guard: once any tool matches every query word, weaker partial
	// matches are noise that inflates the payload (the whole point of search
	// over list_tools). Near-misses survive it — a tool that missed exactly
	// one word of a query at least nearMissMinQueryWords long is a candidate
	// the caller asked for, not noise, and hiding it behind a full-coverage
	// match is how a model ends up never seeing the tool it needs. Matches are
	// already sorted, so full-coverage hits still rank above every near-miss.
	full := 0
	for _, m := range matches {
		if m.score >= fullCoverageBonus {
			full++
		}
	}
	switch {
	case full == 0:
		// Nothing covered the whole query; every partial is the fallback.
	case len(queryWords) < nearMissMinQueryWords:
		matches = matches[:full]
	default:
		keep := make([]scored, full, len(matches))
		copy(keep, matches[:full])
		for _, m := range matches[full:] {
			if m.hits == len(queryWords)-1 {
				keep = append(keep, m)
			}
		}
		matches = keep
	}

	results := make([]string, 0, len(matches))
	for _, m := range matches {
		required, optional := parseInputSchema(m.tool.InputSchema)
		results = append(results, formatToolSearchResult(m.server, m.tool.Name, m.tool.Description, required, optional, maxDescChars))
	}
	return results
}
