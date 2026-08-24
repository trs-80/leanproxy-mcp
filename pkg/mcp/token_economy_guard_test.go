package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mmornati/leanproxy-mcp/pkg/reporter"
)

// syntheticServer seeds count tools shaped like a production MCP server:
// ~230-char descriptions and 5-property schemas (mirrors the GitHub server's
// average of ~110 schema bytes/property + prose descriptions).
func seedSyntheticServer(h *Handler, name string, count int) {
	for i := 0; i < count; i++ {
		desc := fmt.Sprintf("Tool %02d on %s: performs a representative operation against the upstream API, supporting filtering, pagination, and structured responses. Use it when the task calls for this operation on %s resources.", i, name, name)
		schema := json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Filter by state"},"per_page":{"type":"number","description":"Results per page (max 100)"},"page":{"type":"number","description":"Page number"}},"required":["owner","repo"]}`))
		seedToolCache(h, name, Tool{
			Name:        fmt.Sprintf("op_%02d_%s", i, []string{"list", "get", "create", "update", "search"}[i%5]),
			Description: desc,
			InputSchema: schema,
		})
	}
}

// TestTokenEconomy_SearchBeatsListChain pins the core token-reduction claim of
// search_tools: targeted single-call discovery must cost a small fraction of
// the old list_servers -> list_tools chain, measured on the actual handler
// output with the same estimator the runtime cost tracker uses.
func TestTokenEconomy_SearchBeatsListChain(t *testing.T) {
	mp := newMockPool()
	mp.servers["github"] = "idle"
	mp.servers["garmin"] = "idle"
	h := NewHandler(mp, nil)
	seedSyntheticServer(h, "github", 41)
	seedSyntheticServer(h, "garmin", 100)
	est := reporter.NewEstimator()

	// Old flow, step 1: list_servers (approximate its result payload).
	serversJSON, _ := json.Marshal([]map[string]interface{}{
		{"name": "github", "status": "healthy", "transport": "stdio", "tool_count": 41},
		{"name": "garmin", "status": "healthy", "transport": "stdio", "tool_count": 100},
	})
	listServersTok := est.EstimateTokens(string(serversJSON))

	// Old flow, step 2: list_tools on the chosen server (full listing).
	listResp := callGatewayTool(t, h, "list_tools", map[string]interface{}{"server_name": "github"})
	listToolsTok := est.EstimateTokens(string(listResp.Result))

	// New flow: one targeted search.
	searchResp := callGatewayTool(t, h, "search_tools", map[string]interface{}{"query": "create github"})
	searchTok := est.EstimateTokens(string(searchResp.Result))

	oldFlow := listServersTok + listToolsTok
	t.Logf("discovery payload tokens: old flow (list_servers %d + list_tools %d) = %d; search_tools = %d; reduction = %.0f%%",
		listServersTok, listToolsTok, oldFlow, searchTok, 100*(1-float64(searchTok)/float64(oldFlow)))

	if searchTok*4 > oldFlow {
		t.Errorf("search_tools (%d tok) should be <25%% of the old discovery chain (%d tok)", searchTok, oldFlow)
	}

	// And the old flow cost a whole extra turn on top of the payload delta —
	// quantify both harnesses with the reporter cost model so the relationship
	// is pinned, not asserted from folklore.
	base := reporter.SessionShape{Turns: 6, FixedPrefix: 1500, GrowthPerTurn: 1200, OutputPerTurn: 250}
	oldShape := base
	oldShape.Turns += 2 // list_servers + list_tools turns
	newShape := base
	newShape.Turns++ // single search turn

	for _, m := range []reporter.HarnessCostModel{reporter.AnthropicCachedModel(), reporter.FlatRateModel(2.0)} {
		oldUSD := m.Cost(oldShape)
		newUSD := m.Cost(newShape)
		t.Logf("%s: old discovery flow $%.6f vs search flow $%.6f (%.1f%% cheaper)", m.Name, oldUSD, newUSD, 100*(1-newUSD/oldUSD))
		if newUSD >= oldUSD {
			t.Errorf("%s: single-turn discovery must be cheaper: new=%v old=%v", m.Name, newUSD, oldUSD)
		}
	}
}

// TestTokenEconomy_GatewayPrefixBounded pins the size of the always-paid
// tools/list prefix (the 3 gateway tool definitions): adding search_tools must
// not balloon the fixed per-conversation cost. Budget chosen ~25% above the
// current measured size to catch accidental prose creep in review, not drift.
func TestTokenEconomy_GatewayPrefixBounded(t *testing.T) {
	h := NewHandler(newMockPool(), nil)
	resp, err := h.handleToolsList(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1")})
	if err != nil {
		t.Fatalf("handleToolsList: %v", err)
	}
	est := reporter.NewEstimator()
	tok := est.EstimateTokens(string(resp.Result))
	t.Logf("gateway tools/list prefix: %d bytes, %d tokens", len(resp.Result), tok)

	const budgetTok = 700
	if tok > budgetTok {
		t.Errorf("gateway tools/list prefix = %d tokens, budget %d — trim tool descriptions/schemas", tok, budgetTok)
	}
}

// TestTokenEconomy_TruncationCapsResultTokens pins the max_response_chars
// lever: a capped result must estimate at roughly cap/4 tokens regardless of
// upstream size.
func TestTokenEconomy_TruncationCapsResultTokens(t *testing.T) {
	est := reporter.NewEstimator()
	huge, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("data ", 20000)}}, // ~100KB
	})
	capped := truncateToolResult(huge, 2000, true)

	hugeTok := est.EstimateTokens(string(huge))
	cappedTok := est.EstimateTokens(string(capped))
	t.Logf("result tokens: uncapped %d -> capped %d", hugeTok, cappedTok)
	if cappedTok > 700 { // 2000 chars ≈ 500 tok + marker/JSON overhead
		t.Errorf("capped result too large: %d tokens", cappedTok)
	}
	if hugeTok < cappedTok*30 {
		t.Errorf("expected order-of-magnitude reduction, got %d -> %d", hugeTok, cappedTok)
	}
}
