package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTruncStatsHandler returns a handler with one capped tool ("cbm/big_dump",
// 1000 chars) whose upstream returns a payload of the given size.
func newTruncStatsHandler(logger *slog.Logger, payloadChars int) *Handler {
	mp := newMockPool()
	mp.servers["cbm"] = "idle"
	mp.sendRequestFunc = func(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration) (*pool.Response, error) {
		res, _ := json.Marshal(map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", payloadChars)}},
		})
		return &pool.Response{Result: res}, nil
	}
	h := NewHandler(mp, logger)
	h.SetToolMaxResponseChars("cbm", "big_dump", 1000)
	seedToolCache(h, "cbm", Tool{Name: "big_dump", InputSchema: json.RawMessage(`{"type":"object"}`)})
	return h
}

func TestTruncationStats_RecordsPerToolOnTruncation(t *testing.T) {
	t.Parallel()

	h := newTruncStatsHandler(nil, 5000)

	for i := 0; i < 2; i++ {
		resp := callGatewayTool(t, h, "invoke_tool",
			map[string]interface{}{"server": "cbm", "tool": "big_dump"})
		require.Nil(t, resp.Error)
	}

	stats := h.TruncationStats()
	require.Contains(t, stats, "cbm/big_dump")
	got := stats["cbm/big_dump"]
	assert.Equal(t, int64(2), got.TruncatedCalls)
	assert.Greater(t, got.BytesBefore, got.BytesAfter,
		"before must exceed after when a result was cut")
	// 5000-char payload capped at 1000: each call should save roughly 4000
	// bytes; assert a conservative floor so envelope overhead can't flake it.
	assert.Greater(t, got.BytesBefore-got.BytesAfter, int64(2*3500))
}

func TestTruncationStats_UntruncatedCallsNotRecorded(t *testing.T) {
	t.Parallel()

	h := newTruncStatsHandler(nil, 100) // well under the 1000-char cap

	resp := callGatewayTool(t, h, "invoke_tool",
		map[string]interface{}{"server": "cbm", "tool": "big_dump"})
	require.Nil(t, resp.Error)

	assert.Empty(t, h.TruncationStats())
}

func TestTruncationStats_ReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	h := newTruncStatsHandler(nil, 5000)
	resp := callGatewayTool(t, h, "invoke_tool",
		map[string]interface{}{"server": "cbm", "tool": "big_dump"})
	require.Nil(t, resp.Error)

	first := h.TruncationStats()
	first["cbm/big_dump"] = TruncationStat{TruncatedCalls: 99}
	delete(first, "cbm/big_dump")

	second := h.TruncationStats()
	require.Contains(t, second, "cbm/big_dump")
	assert.Equal(t, int64(1), second["cbm/big_dump"].TruncatedCalls)
}

func TestTruncation_LogsEventWithSizes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTruncStatsHandler(logger, 5000)

	resp := callGatewayTool(t, h, "invoke_tool",
		map[string]interface{}{"server": "cbm", "tool": "big_dump"})
	require.Nil(t, resp.Error)

	logged := buf.String()
	assert.Contains(t, logged, "result truncated")
	assert.Contains(t, logged, "cbm/big_dump")
	assert.Contains(t, logged, "cap=1000")
	assert.Contains(t, logged, "bytes_before=")
	assert.Contains(t, logged, "bytes_after=")
}

func TestTruncation_UntruncatedCallsNotLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newTruncStatsHandler(logger, 100)

	resp := callGatewayTool(t, h, "invoke_tool",
		map[string]interface{}{"server": "cbm", "tool": "big_dump"})
	require.Nil(t, resp.Error)

	assert.NotContains(t, buf.String(), "result truncated")
}

// The direct tools/call path and the cached-result path must feed the same
// stats as invoke_tool — cap parity bugs historically split along these paths.
func TestTruncationStats_CountsDirectAndCachedPaths(t *testing.T) {
	t.Parallel()

	h := newTruncStatsHandler(nil, 5000)
	h.SetToolCacheTTL("cbm", "big_dump", time.Minute)

	directArgs, err := json.Marshal(map[string]interface{}{
		"name": "cbm_big_dump", "arguments": map[string]interface{}{},
	})
	require.NoError(t, err)

	for i := 0; i < 2; i++ { // second call is a result-cache hit
		resp, err := h.handleToolsCall(context.Background(),
			&Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Params: directArgs})
		require.NoError(t, err)
		require.Nil(t, resp.Error)
	}

	stats := h.TruncationStats()
	require.Contains(t, stats, "cbm/big_dump")
	assert.Equal(t, int64(2), stats["cbm/big_dump"].TruncatedCalls,
		"cache-hit truncations must be counted too")
}
