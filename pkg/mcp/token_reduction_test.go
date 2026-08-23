package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// seedToolCache installs tools directly into the handler cache so discovery
// tests do not depend on live server round trips.
func seedToolCache(h *Handler, server string, tools ...Tool) {
	h.toolCache.mu.Lock()
	defer h.toolCache.mu.Unlock()
	h.toolCache.tools[server] = append(h.toolCache.tools[server], tools...)
}

func callGatewayTool(t *testing.T, h *Handler, name string, args map[string]interface{}) *Response {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	resp, err := h.handleLeanproxyTool(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")}, ToolsCallParams{
		Name:      name,
		Arguments: argsJSON,
	})
	if err != nil {
		t.Fatalf("handleLeanproxyTool(%s): %v", name, err)
	}
	return resp
}

func resultText(t *testing.T, resp *Response) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %v", resp.Error)
	}
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var sb strings.Builder
	for _, c := range result.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func issueSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"labels":{"type":"array"}},"required":["owner","repo"]}`)
}

func TestSearchTools_MatchesAcrossServersWithSignatures(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github",
		Tool{Name: "create_issue", Description: "Create a new issue in a repository", InputSchema: issueSchema()},
		Tool{Name: "list_pulls", Description: "List pull requests", InputSchema: json.RawMessage(`{}`)},
	)
	seedToolCache(h, "gitlab",
		Tool{Name: "issue_create", Description: "Open an issue on a GitLab project", InputSchema: json.RawMessage(`{}`)},
	)

	text := resultText(t, callGatewayTool(t, h, "search_tools", map[string]interface{}{"query": "issue create"}))

	if !strings.Contains(text, "github_create_issue") {
		t.Errorf("expected github_create_issue in results, got:\n%s", text)
	}
	if !strings.Contains(text, "gitlab_issue_create") {
		t.Errorf("expected gitlab_issue_create in results, got:\n%s", text)
	}
	if strings.Contains(text, "list_pulls") {
		t.Errorf("list_pulls should not match 'issue create', got:\n%s", text)
	}
	// invocation-ready signature: required params inline
	if !strings.Contains(text, "[owner: string, repo: string]") {
		t.Errorf("expected required-param signature inline, got:\n%s", text)
	}
}

func TestSearchTools_ServerFilterAndLimit(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github",
		Tool{Name: "a_tool", Description: "alpha", InputSchema: json.RawMessage(`{}`)},
		Tool{Name: "b_tool", Description: "beta", InputSchema: json.RawMessage(`{}`)},
		Tool{Name: "c_tool", Description: "gamma", InputSchema: json.RawMessage(`{}`)},
	)
	seedToolCache(h, "garmin", Tool{Name: "d_tool", Description: "delta", InputSchema: json.RawMessage(`{}`)})

	text := resultText(t, callGatewayTool(t, h, "search_tools", map[string]interface{}{"server": "github", "limit": 2}))

	if strings.Contains(text, "garmin_") {
		t.Errorf("server filter leaked garmin tools:\n%s", text)
	}
	if !strings.Contains(text, "1 more") {
		t.Errorf("expected truncation note for limit=2 of 3, got:\n%s", text)
	}
}

func TestSearchTools_NoMatches(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github", Tool{Name: "create_issue", Description: "Create issue", InputSchema: json.RawMessage(`{}`)})

	text := resultText(t, callGatewayTool(t, h, "search_tools", map[string]interface{}{"query": "quantum flux"}))
	if !strings.Contains(text, "No tools matching") {
		t.Errorf("expected no-match message, got:\n%s", text)
	}
}

func TestInvokeTool_TruncatesResultToMaxResponseChars(t *testing.T) {
	mp := newMockPool()
	mp.servers["github"] = "idle"
	long := strings.Repeat("x", 5000)
	resultJSON, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": long}},
	})
	mp.requestResult = &MockRequestResult{Result: resultJSON}
	h := NewHandler(mp, nil)
	h.pool.MarkServerMCPInitialized("github")

	resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{
		"server": "github", "tool": "get_file", "max_response_chars": 300,
	})
	text := resultText(t, resp)

	if !strings.Contains(text, "[leanproxy: truncated, 300 of 5000 chars shown") {
		t.Errorf("expected truncation marker, got tail: %q", text[max(0, len(text)-120):])
	}
	if len(text) > 300+200 {
		t.Errorf("truncated result too large: %d chars", len(text))
	}
}

func TestInvokeTool_NoTruncationWhenUnderCap(t *testing.T) {
	mp := newMockPool()
	mp.servers["github"] = "idle"
	resultJSON, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": "short"}},
	})
	mp.requestResult = &MockRequestResult{Result: resultJSON}
	h := NewHandler(mp, nil)
	h.pool.MarkServerMCPInitialized("github")

	resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{
		"server": "github", "tool": "get_file", "max_response_chars": 300,
	})
	if text := resultText(t, resp); text != "short" {
		t.Errorf("expected untouched result, got %q", text)
	}
}

func TestInvokeTool_DefaultCapFromConfig(t *testing.T) {
	mp := newMockPool()
	mp.servers["github"] = "idle"
	long := strings.Repeat("y", 1000)
	resultJSON, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": long}},
	})
	mp.requestResult = &MockRequestResult{Result: resultJSON}
	h := NewHandler(mp, nil)
	h.SetDefaultMaxResponseChars(400)
	h.pool.MarkServerMCPInitialized("github")

	resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{"server": "github", "tool": "get_file"})
	if text := resultText(t, resp); !strings.Contains(text, "truncated, 400 of 1000") {
		t.Errorf("expected server-side default cap to apply, got tail: %q", text[max(0, len(text)-120):])
	}
}

func TestTruncateToolResult_PassThroughUnparseable(t *testing.T) {
	raw := json.RawMessage(`{"custom":"shape","not":"content-blocks"}`)
	if got := truncateToolResult(raw, 5); string(got) != string(raw) {
		t.Errorf("unparseable result must pass through untouched, got %s", got)
	}
}

func TestSuggestTools_SameServerCloseMatch(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github",
		Tool{Name: "create_issue", Description: "Create a new issue", InputSchema: issueSchema()},
		Tool{Name: "delete_branch", Description: "Delete a branch", InputSchema: json.RawMessage(`{}`)},
	)

	got := h.suggestTools("github", "create_issues", 3)
	if !strings.Contains(got, "github_create_issue") {
		t.Errorf("expected create_issue suggested for create_issues, got: %q", got)
	}
	if !strings.Contains(got, "[owner: string, repo: string]") {
		t.Errorf("suggestion should carry an invocation-ready signature, got: %q", got)
	}
	if strings.Contains(got, "delete_branch") {
		t.Errorf("unrelated tool suggested: %q", got)
	}
}

func TestSuggestTools_FallsBackToOtherServers(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "gitlab", Tool{Name: "create_issue", Description: "Open issue", InputSchema: json.RawMessage(`{}`)})

	got := h.suggestTools("github", "create_issue", 3)
	if !strings.Contains(got, "gitlab_create_issue") {
		t.Errorf("expected cross-server fallback suggestion, got: %q", got)
	}
}

func TestSuggestTools_NoMatchReturnsEmpty(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github", Tool{Name: "zzz", Description: "unrelated", InputSchema: json.RawMessage(`{}`)})
	if got := h.suggestTools("github", "create_issue", 3); got != "" {
		t.Errorf("expected empty suggestions, got %q", got)
	}
}

func TestToolsListStubs_DescriptionsCapped(t *testing.T) {
	mp := newMockPool()
	h := NewHandler(mp, nil)
	h.EnableLazyLoading(0)
	long := strings.Repeat("d", 500)
	seedToolCache(h, "github", Tool{Name: "verbose_tool", Description: long, InputSchema: json.RawMessage(`{}`)})

	resp, err := h.handleToolsList(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")})
	if err != nil {
		t.Fatalf("handleToolsList: %v", err)
	}
	var result ToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name == "github_verbose_tool" {
			if len(tool.Description) > stubDescChars {
				t.Errorf("stub description not capped: %d chars > %d", len(tool.Description), stubDescChars)
			}
			return
		}
	}
	t.Fatal("github_verbose_tool stub not found in tools/list")
}

// TestToolsCall_RoutesSearchTools exercises search_tools through the real
// tools/call entrypoint — the path MCP clients hit. Regression test for the
// dispatch gap where search_tools fell through to parseToolName and was
// forwarded to a nonexistent server named "search".
func TestToolsCall_RoutesSearchTools(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github", Tool{Name: "create_issue", Description: "Create issue", InputSchema: issueSchema()})

	params, _ := json.Marshal(ToolsCallParams{Name: "search_tools", Arguments: json.RawMessage(`{"query":"issue"}`)})
	resp, err := h.handleToolsCall(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: params})
	if err != nil {
		t.Fatalf("handleToolsCall: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("search_tools via tools/call errored: %v", resp.Error)
	}
	if text := resultText(t, resp); !strings.Contains(text, "github_create_issue") {
		t.Errorf("expected search results, got: %s", text)
	}
}

// TestSearchTools_PartialMatchFallback pins the scored-OR recall behavior: a
// query where no tool matches every word must still return the best partial
// matches (the all-words-AND matcher returned nothing here, stranding real
// sessions into a full list_tools fallback).
func TestSearchTools_PartialMatchFallback(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "cbm",
		Tool{Name: "trace_path", Description: "Trace call paths through the graph", InputSchema: json.RawMessage(`{}`)},
		Tool{Name: "list_projects", Description: "List indexed projects", InputSchema: json.RawMessage(`{}`)},
	)

	text := resultText(t, callGatewayTool(t, h, "search_tools", map[string]interface{}{"query": "trace path callers"}))
	if !strings.Contains(text, "cbm_trace_path") {
		t.Errorf("partial match should surface trace_path, got:\n%s", text)
	}
	if strings.Contains(text, "list_projects") {
		t.Errorf("zero-hit tool must not appear, got:\n%s", text)
	}
}

// TestSearchTools_PrecisionDropsPartialsWhenFullExists: with a full-coverage
// match available, single-word noise matches are excluded from the payload.
func TestSearchTools_PrecisionDropsPartialsWhenFullExists(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	seedToolCache(h, "github",
		Tool{Name: "create_issue", Description: "Create a new issue", InputSchema: json.RawMessage(`{}`)},
		Tool{Name: "create_branch", Description: "Create a branch", InputSchema: json.RawMessage(`{}`)},
	)

	text := resultText(t, callGatewayTool(t, h, "search_tools", map[string]interface{}{"query": "create issue"}))
	if !strings.Contains(text, "github_create_issue") {
		t.Errorf("full match missing:\n%s", text)
	}
	if strings.Contains(text, "create_branch") {
		t.Errorf("partial match should be dropped when full matches exist:\n%s", text)
	}
}

// TestToolsListStubs_SchemasAreValidToolSchemas: every stub must carry
// input_schema.type == "object" — the Anthropic API rejects anything else and
// clients silently drop the tool, making lazy mode invisibly toolless.
func TestToolsListStubs_SchemasAreValidToolSchemas(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	h.EnableLazyLoading(0)
	seedToolCache(h, "github", Tool{Name: "a_tool", Description: "d", InputSchema: json.RawMessage(`{}`)})

	resp, err := h.handleToolsList(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")})
	if err != nil {
		t.Fatal(err)
	}
	var result ToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		var schema struct {
			Type string `json:"type"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if err := json.Unmarshal(raw, &schema); err != nil || schema.Type != "object" {
			t.Errorf("tool %s: input schema %s lacks type object", tool.Name, raw)
		}
	}
}

// TestToolsListStubs_CompactSchemasKeepParams: stubs must expose param names,
// types, and required — without them the model guesses arguments (measured:
// a no-schema list_projects stub triggered 23 fallback calls in one session).
func TestToolsListStubs_CompactSchemasKeepParams(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	h.EnableLazyLoading(0)
	seedToolCache(h, "cbm", Tool{
		Name:        "list_projects",
		Description: "List indexed projects with node counts",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number","description":"very long prose that must be dropped"},"filter":{"type":"string"}},"required":["limit"]}`),
	})

	resp, err := h.handleToolsList(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")})
	if err != nil {
		t.Fatal(err)
	}
	var result ToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "cbm_list_projects" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Type       string                       `json:"type"`
			Properties map[string]map[string]string `json:"properties"`
			Required   []string                     `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("stub schema unparseable: %s", raw)
		}
		if schema.Type != "object" || schema.Properties["limit"]["type"] != "number" || schema.Properties["filter"]["type"] != "string" {
			t.Errorf("stub schema lost params: %s", raw)
		}
		if len(schema.Required) != 1 || schema.Required[0] != "limit" {
			t.Errorf("stub schema lost required list: %s", raw)
		}
		if strings.Contains(string(raw), "prose") {
			t.Errorf("stub schema kept prose: %s", raw)
		}
		return
	}
	t.Fatal("stub not found")
}
