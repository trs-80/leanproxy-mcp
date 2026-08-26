package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func usagePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "toolusage.json")
}

func stubByName(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("stub %s not in tools/list", name)
	return Tool{}
}

func adaptiveHandler(t *testing.T, path string, window time.Duration) *Handler {
	t.Helper()
	h := NewHandler(newMockPool(), nil)
	h.EnableLazyLoading(0)
	h.EnableUsageTracking(path)
	h.SetAdaptiveStubWindow("cbm", window)
	seedToolCache(h, "cbm",
		Tool{Name: "hot_tool", Description: "used constantly", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		Tool{Name: "cold_tool", Description: "never used", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	)
	return h
}

func writeUsageFile(t *testing.T, path string, records map[string]usageRecord) {
	t.Helper()
	data, err := json.Marshal(records)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
}

// A tool with no usage history is never demoted — it may be brand new, and
// it gets a full window (counted from first sight) before demotion.
func TestAdaptiveStubs_FreshToolKeepsFullStub(t *testing.T) {
	t.Parallel()

	h := adaptiveHandler(t, usagePath(t), time.Hour)

	tools := lazyToolsList(t, h)

	cold := stubByName(t, tools, "cbm_cold_tool")
	assert.NotEmpty(t, cold.Description)
	assert.Contains(t, string(cold.InputSchema), `"properties"`)
}

// A tool unused beyond the window renders name-only: no description, bare
// object schema. A recently used tool keeps its full stub.
func TestAdaptiveStubs_StaleToolDemotedUsedToolKept(t *testing.T) {
	t.Parallel()

	path := usagePath(t)
	old := time.Now().Add(-48 * time.Hour).Unix()
	writeUsageFile(t, path, map[string]usageRecord{
		"cbm/cold_tool": {FirstSeen: old},
		"cbm/hot_tool":  {FirstSeen: old, LastUsed: time.Now().Unix()},
	})
	h := adaptiveHandler(t, path, time.Hour)

	tools := lazyToolsList(t, h)

	cold := stubByName(t, tools, "cbm_cold_tool")
	assert.Empty(t, cold.Description, "stale tool must render name-only")
	assert.JSONEq(t, `{"type":"object"}`, string(cold.InputSchema))
	hot := stubByName(t, tools, "cbm_hot_tool")
	assert.NotEmpty(t, hot.Description)
	assert.Contains(t, string(hot.InputSchema), `"properties"`)
}

// Invoking a demoted tool restores its full stub on the next tools/list.
func TestAdaptiveStubs_InvocationRestoresFullStub(t *testing.T) {
	t.Parallel()

	path := usagePath(t)
	writeUsageFile(t, path, map[string]usageRecord{
		"cbm/cold_tool": {FirstSeen: time.Now().Add(-48 * time.Hour).Unix()},
	})
	h := adaptiveHandler(t, path, time.Hour)
	h.pool.(*mockPool).servers["cbm"] = "idle"

	demoted := stubByName(t, lazyToolsList(t, h), "cbm_cold_tool")
	require.Empty(t, demoted.Description)

	resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{"server": "cbm", "tool": "cold_tool"})
	require.Nil(t, resp.Error)

	restored := stubByName(t, lazyToolsList(t, h), "cbm_cold_tool")
	assert.NotEmpty(t, restored.Description, "one invocation must restore the full stub")
}

// A demoted tool stays fully callable — demotion is presentation only.
func TestAdaptiveStubs_DemotedToolStillCallable(t *testing.T) {
	t.Parallel()

	path := usagePath(t)
	writeUsageFile(t, path, map[string]usageRecord{
		"cbm/cold_tool": {FirstSeen: time.Now().Add(-48 * time.Hour).Unix()},
	})
	h := adaptiveHandler(t, path, time.Hour)
	h.pool.(*mockPool).servers["cbm"] = "idle"

	resp := callGatewayTool(t, h, "invoke_tool", map[string]interface{}{"server": "cbm", "tool": "cold_tool"})

	require.Nil(t, resp.Error)
}

// Usage survives a restart: a new handler reading the same file sees the
// records the previous one wrote.
func TestAdaptiveStubs_UsagePersistsAcrossHandlers(t *testing.T) {
	t.Parallel()

	path := usagePath(t)
	h1 := adaptiveHandler(t, path, time.Hour)
	h1.pool.(*mockPool).servers["cbm"] = "idle"
	resp := callGatewayTool(t, h1, "invoke_tool", map[string]interface{}{"server": "cbm", "tool": "hot_tool"})
	require.Nil(t, resp.Error)
	h1.usage.maybeSave(true)

	h2 := adaptiveHandler(t, path, time.Hour)
	rec, ok := h2.usage.record("cbm/hot_tool")

	require.True(t, ok, "usage record must survive restart")
	assert.NotZero(t, rec.LastUsed)
}

// Servers without a configured window are untouched regardless of history.
func TestAdaptiveStubs_UnconfiguredServerNeverDemotes(t *testing.T) {
	t.Parallel()

	path := usagePath(t)
	writeUsageFile(t, path, map[string]usageRecord{
		"other/tool_a": {FirstSeen: time.Now().Add(-480 * time.Hour).Unix()},
	})
	h := NewHandler(newMockPool(), nil)
	h.EnableLazyLoading(0)
	h.EnableUsageTracking(path)
	seedToolCache(h, "other", Tool{Name: "tool_a", Description: "desc", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)})

	tool := stubByName(t, lazyToolsList(t, h), "other_tool_a")

	assert.NotEmpty(t, tool.Description)
}
