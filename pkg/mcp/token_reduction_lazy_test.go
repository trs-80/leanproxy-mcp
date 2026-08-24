package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/pool"
)

func lazyToolsList(t *testing.T, h *Handler) []Tool {
	t.Helper()
	resp, err := h.handleToolsList(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")})
	if err != nil {
		t.Fatalf("handleToolsList: %v", err)
	}
	var result ToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return result.Tools
}

// Lazy mode exposes every upstream tool directly, so the gateway wrappers are
// dead weight paid on every turn (measured on Bob: ~620 tokens/turn, zero
// wrapper calls across all recorded sessions).
func TestToolsListLazy_OmitsGatewayWrappers(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	h.EnableLazyLoading(0)
	seedToolCache(h, "cbm", Tool{Name: "a", Description: "d", InputSchema: json.RawMessage(`{}`)})

	for _, tool := range lazyToolsList(t, h) {
		switch tool.Name {
		case "list_tools", "invoke_tool", "search_tools":
			t.Errorf("lazy tools/list should not include wrapper %s", tool.Name)
		}
	}
}

// Non-lazy mode must still advertise the wrappers: they are the only way to
// reach upstream tools there.
func TestToolsListNonLazy_KeepsGatewayWrappers(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	names := map[string]bool{}
	for _, tool := range lazyToolsList(t, h) {
		names[tool.Name] = true
	}
	for _, w := range []string{"list_tools", "invoke_tool", "search_tools"} {
		if !names[w] {
			t.Errorf("non-lazy tools/list missing %s", w)
		}
	}
}

// Identical caches must render identically across sessions (map iteration
// order would otherwise shuffle tools and defeat provider prompt caching).
func TestToolsListLazy_DeterministicOrder(t *testing.T) {
	build := func() string {
		h := NewHandler(newMockPool(), nil)
		h.EnableLazyLoading(0)
		for _, s := range []string{"zeta", "alpha", "mid"} {
			seedToolCache(h, s, Tool{Name: "b", InputSchema: json.RawMessage(`{}`)}, Tool{Name: "a", InputSchema: json.RawMessage(`{}`)})
		}
		var names []string
		for _, tool := range lazyToolsList(t, h) {
			names = append(names, tool.Name)
		}
		return strings.Join(names, ",")
	}
	want := "alpha_a,alpha_b,mid_a,mid_b,zeta_a,zeta_b"
	for i := 0; i < 5; i++ {
		if got := build(); got != want {
			t.Fatalf("order not deterministic/sorted: got %s want %s", got, want)
		}
	}
}

// Per-server include/exclude lists drop tools from the cache — and therefore
// from tools/list stubs, search_tools, and list_tools — so a 15-tool server can
// be trimmed to the handful actually used.
func TestToolFilter_IncludeExclude(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	h.SetToolFilter("cbm", []string{"search_graph", "trace_path"}, nil)
	h.SetToolFilter("gh", nil, []string{"delete_repo"})

	h.storeTools("cbm", []Tool{{Name: "search_graph"}, {Name: "trace_path"}, {Name: "manage_adr"}})
	h.storeTools("gh", []Tool{{Name: "list_issues"}, {Name: "delete_repo"}})

	got := map[string]bool{}
	h.toolCache.mu.RLock()
	for s, tools := range h.toolCache.tools {
		for _, tl := range tools {
			got[s+"_"+tl.Name] = true
		}
	}
	h.toolCache.mu.RUnlock()

	for _, keep := range []string{"cbm_search_graph", "cbm_trace_path", "gh_list_issues"} {
		if !got[keep] {
			t.Errorf("filter dropped %s", keep)
		}
	}
	for _, drop := range []string{"cbm_manage_adr", "gh_delete_repo"} {
		if got[drop] {
			t.Errorf("filter kept %s", drop)
		}
	}
}

// Per-tool response caps: config cap beats the global default; an explicit
// max_response_chars argument beats both.
func TestResponseCap_PerToolOverridesDefault(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	h.SetDefaultMaxResponseChars(4000)
	h.SetToolMaxResponseChars("cbm", "index_status", 1000)

	if got := h.responseCapFor("cbm", "index_status", 0); got != 1000 {
		t.Errorf("per-tool cap = %d, want 1000", got)
	}
	if got := h.responseCapFor("cbm", "search_graph", 0); got != 4000 {
		t.Errorf("default cap = %d, want 4000", got)
	}
	if got := h.responseCapFor("cbm", "index_status", 8000); got != 8000 {
		t.Errorf("explicit cap = %d, want 8000", got)
	}
}

func TestToolsCallLazy_AppliesPerToolCap(t *testing.T) {
	mp := newMockPool()
	mp.servers["cbm"] = "idle"
	resultJSON, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 5000)}},
	})
	mp.requestResult = &MockRequestResult{Result: resultJSON}
	h := NewHandler(mp, nil)
	h.pool.MarkServerMCPInitialized("cbm")
	h.SetToolMaxResponseChars("cbm", "index_status", 500)

	args, _ := json.Marshal(map[string]interface{}{"name": "cbm_index_status", "arguments": map[string]interface{}{}})
	resp, err := h.handleToolsCall(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: args})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, resp)
	if !strings.Contains(text, "truncated, 500 of 5000") {
		t.Errorf("per-tool cap not applied on direct tools/call: %d chars", len(text))
	}
}

func TestTruncateDescription_WordBoundary(t *testing.T) {
	d := "Search the graph for symbols by name and kind, returning matches ranked by relevance"
	got := truncateDescription(d, 40)
	if len(got) > 40 {
		t.Errorf("over cap: %d %q", len(got), got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") || !strings.HasSuffix(got, "…") {
		t.Errorf("expected word-boundary cut with ellipsis, got %q", got)
	}
	if strings.Contains(got, "kind, re") {
		t.Errorf("cut mid-word: %q", got)
	}
	if got := truncateDescription("short", 40); got != "short" {
		t.Errorf("short description altered: %q", got)
	}
}

func TestCompactSchema_KeepsShortEnums(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{
		"mode":{"type":"string","enum":["calls","imports"]},
		"big":{"type":"string","enum":["aaaaaaaaaaaaaaaaaaaa","bbbbbbbbbbbbbbbbbbbb","cccccccccccccccccccc","dddddddddddddddddddd","eeeeeeeeeeeeeeeeeeee","ffffffffffffffffffff","gggggggggggggggggggg"]}
	}}`)
	out := string(compactSchema(in))
	if !strings.Contains(out, `"enum":["calls","imports"]`) {
		t.Errorf("short enum dropped: %s", out)
	}
	if strings.Contains(out, "aaaaaaaaaaaaaaaaaaaa") {
		t.Errorf("long enum kept: %s", out)
	}
}

func TestErrorHints_Compact(t *testing.T) {
	got := FormatErrorWithHint("tool invocation failed: server github is not running", "github", "list_issues")
	for _, bad := range []string{"💡", "📋"} {
		if strings.Contains(got, bad) {
			t.Errorf("emoji in error text: %q", got)
		}
	}
	if !strings.Contains(got, "Hint:") {
		t.Errorf("expected hint, got %q", got)
	}
	// generic "failed" catch-all must not fire on every proxied error
	if got := EnrichError("tool invocation failed: boom"); strings.Contains(got, "Hint:") {
		t.Errorf("catch-all hint fired: %q", got)
	}
}

func TestSearchToolsDescription_NotStale(t *testing.T) {
	def := GetToolDefinition("search_tools")
	if strings.Contains(string(def.InputSchema), "all words must match") {
		t.Error("search_tools query description still says all words must match (search is scored-OR)")
	}
}

func TestTruncateToolResult_SkipsWhenSavingsBelowMarker(t *testing.T) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 1050)}},
	})
	if out := truncateToolResult(raw, 1000); string(out) != string(raw) {
		t.Errorf("truncated a result only 50 chars over the cap (marker would cost more)")
	}
	if out := truncateToolResult(raw, 500); string(out) == string(raw) {
		t.Errorf("expected truncation when well over cap")
	}
}

func TestTruncateToolResult_KeepsTrailingNonTextBlocks(t *testing.T) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": strings.Repeat("a", 2000)},
			{"type": "text", "text": strings.Repeat("b", 2000)},
			{"type": "image", "data": "xyz", "mimeType": "image/png"},
		},
	})
	out := truncateToolResult(raw, 500)
	var result struct {
		Content []map[string]interface{} `json:"content"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	sawImage := false
	for _, b := range result.Content {
		if b["type"] == "image" {
			sawImage = true
		}
		if txt, ok := b["text"].(string); ok && strings.HasPrefix(txt, "b") {
			t.Errorf("second text block should be dropped entirely, got %d chars", len(txt))
		}
	}
	if !sawImage {
		t.Error("trailing image block was dropped by truncation")
	}
}

func TestTruncateToolResult_PreservesTopLevelFields(t *testing.T) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content":           []map[string]interface{}{{"type": "text", "text": strings.Repeat("x", 2000)}},
		"structuredContent": map[string]interface{}{"answer": 42},
		"_meta":             map[string]interface{}{"trace": "abc"},
		"isError":           false,
	})
	out := truncateToolResult(raw, 500)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"structuredContent", "_meta", "isError"} {
		if _, ok := result[field]; !ok {
			t.Errorf("truncation dropped top-level field %q", field)
		}
	}
	if !strings.Contains(string(result["content"]), "truncated") {
		t.Error("content was not truncated")
	}
}

// Discovery filters must also gate dispatch: docs call include/exclude an
// allowlist/denylist, so an excluded tool reachable by guessing its name
// would be a policy bypass.
func TestToolFilter_BlocksDispatch(t *testing.T) {
	sent := 0
	mp := newMockPool()
	mp.servers["gh"] = "idle"
	mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
		if method == MethodToolsCall {
			sent++
		}
		res, _ := json.Marshal(map[string]interface{}{"content": []map[string]string{{"type": "text", "text": "ok"}}})
		return &pool.Response{Result: res}, nil
	}
	h := NewHandler(mp, nil)
	h.pool.MarkServerMCPInitialized("gh")
	h.SetToolFilter("gh", nil, []string{"delete_repo"})
	seedToolCache(h, "gh", Tool{Name: "list_issues", InputSchema: json.RawMessage(`{}`)})

	// Direct tools/call path.
	args, _ := json.Marshal(map[string]interface{}{"name": "gh_delete_repo", "arguments": map[string]interface{}{}})
	resp, err := h.handleToolsCall(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: args})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "not exposed") {
		t.Errorf("direct call to excluded tool not blocked: %+v", resp)
	}

	// invoke_tool path.
	resp = callGatewayTool(t, h, "invoke_tool", map[string]interface{}{"server": "gh", "tool": "delete_repo"})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "not exposed") {
		t.Errorf("invoke_tool of excluded tool not blocked: %+v", resp)
	}
	if sent != 0 {
		t.Errorf("excluded tool reached the upstream server %d times", sent)
	}

	// Non-excluded tool still dispatches on both paths.
	if resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{"server": "gh", "tool": "list_issues"}); resp.Error != nil {
		t.Errorf("allowed tool blocked: %+v", resp.Error)
	}
	if sent != 1 {
		t.Errorf("allowed tool dispatched %d times, want 1", sent)
	}
}

// The truncation marker tells the model to raise max_response_chars; on the
// lazy direct path that argument must be honored (and stripped before the
// call reaches the upstream tool, whose schema does not know it).
func TestToolsCallLazy_HonorsAndStripsMaxResponseChars(t *testing.T) {
	var upstreamArgs string
	mp := newMockPool()
	mp.servers["cbm"] = "idle"
	mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
		var p ToolsCallParams
		_ = json.Unmarshal(params, &p)
		upstreamArgs = string(p.Arguments)
		res, _ := json.Marshal(map[string]interface{}{"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 5000)}}})
		return &pool.Response{Result: res}, nil
	}
	h := NewHandler(mp, nil)
	h.pool.MarkServerMCPInitialized("cbm")

	args, _ := json.Marshal(map[string]interface{}{
		"name":      "cbm_index_status",
		"arguments": map[string]interface{}{"project": "p", "max_response_chars": 600},
	})
	resp, err := h.handleToolsCall(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: args})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, resp)
	if !strings.Contains(text, "truncated, 600 of 5000") {
		t.Errorf("max_response_chars not honored on direct path: %.120s", text)
	}
	if strings.Contains(upstreamArgs, "max_response_chars") {
		t.Errorf("max_response_chars leaked to upstream tool: %s", upstreamArgs)
	}
	if !strings.Contains(upstreamArgs, "project") {
		t.Errorf("real arguments lost: %s", upstreamArgs)
	}
}
