package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textEnvelope builds an MCP tools/call result with a single text block.
func textEnvelope(t *testing.T, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return raw
}

// A configured cap must produce byte-identical truncation on both dispatch
// paths (direct tools/call and invoke_tool). Every historical cap bug in this
// package was one path getting a fix the other missed.
func TestResponseCap_ParityBetweenDirectAndInvokePaths(t *testing.T) {
	t.Parallel()

	newCappedHandler := func() *Handler {
		mp := newMockPool()
		mp.servers["cbm"] = "idle"
		mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
			res, _ := json.Marshal(map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 5000)}},
			})
			return &pool.Response{Result: res}, nil
		}
		h := NewHandler(mp, nil)
		h.SetToolMaxResponseChars("cbm", "big_dump", 1000)
		seedToolCache(h, "cbm", Tool{Name: "big_dump", InputSchema: json.RawMessage(`{"type":"object"}`)})
		return h
	}

	directArgs, err := json.Marshal(map[string]interface{}{"name": "cbm_big_dump", "arguments": map[string]interface{}{}})
	require.NoError(t, err)
	directResp, err := newCappedHandler().handleToolsCall(context.Background(),
		&Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: directArgs})
	require.NoError(t, err)
	require.Nil(t, directResp.Error)

	invokeResp := callGatewayTool(t, newCappedHandler(), "invoke_tool",
		map[string]interface{}{"server": "cbm", "tool": "big_dump"})
	require.Nil(t, invokeResp.Error)

	assert.Contains(t, string(directResp.Result), "truncated, 1000 of 5000 chars shown",
		"direct path must apply the configured cap")
	assert.JSONEq(t, string(directResp.Result), string(invokeResp.Result),
		"the two dispatch paths must truncate identically")
}

// truncateToolResult decides purely on sizes: at or under cap+slack the input
// passes through untouched, over it the text is cut and a marker appended.
func TestTruncateToolResult_CapBoundary(t *testing.T) {
	t.Parallel()

	const maxChars = 300 // slack is truncationMarkerSlack (100)

	tests := []struct {
		name         string
		textLen      int
		wantTruncate bool
	}{
		{"text well under cap passes through", 100, false},
		{"text exactly at cap plus slack passes through", maxChars + truncationMarkerSlack, false},
		{"text one over cap plus slack truncates", maxChars + truncationMarkerSlack + 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := textEnvelope(t, strings.Repeat("a", tt.textLen))

			got := truncateToolResult(raw, maxChars, false)

			if tt.wantTruncate {
				assert.Contains(t, string(got), "[leanproxy: truncated,")
				var envelope struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				require.NoError(t, json.Unmarshal(got, &envelope))
				require.Len(t, envelope.Content, 2, "want the cut text block plus the marker block")
				assert.Len(t, envelope.Content[0].Text, maxChars, "text must be cut to the cap")
			} else {
				assert.Equal(t, raw, got, "under-cap result must pass through byte-identical")
			}
		})
	}
}

// Unparseable payloads are never rewritten, no matter how far over the cap:
// the proxy must not corrupt what it does not understand.
func TestTruncateToolResult_UnparseableOverCapPassesThrough(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage("this is not json " + strings.Repeat("x", 2000))

	got := truncateToolResult(raw, 300, false)

	assert.Equal(t, raw, got)
}

func BenchmarkTruncateToolResult_UnderCapFastPath(b *testing.B) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 1000)}},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		truncateToolResult(raw, 4000, false)
	}
}

func BenchmarkTruncateToolResult_OverCap(b *testing.B) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", 20000)}},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		truncateToolResult(raw, 4000, false)
	}
}
