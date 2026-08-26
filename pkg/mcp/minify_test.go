package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envelopeWithText(t *testing.T, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return raw
}

func textOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	var sb strings.Builder
	for _, c := range env.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// Pretty-printed JSON inside a text block is compacted — same JSON, fewer
// bytes on every turn it is re-read.
func TestMinify_CompactsPrettyPrintedJSONText(t *testing.T) {
	t.Parallel()

	pretty := "{\n  \"items\": [\n    {\"id\": 1},\n    {\"id\": 2}\n  ],\n  \"total\": 2\n}"
	raw := envelopeWithText(t, pretty)

	got := minifyToolResult(raw)

	assert.Equal(t, `{"items":[{"id":1},{"id":2}],"total":2}`, textOf(t, got))
	assert.Less(t, len(got), len(raw))
}

// Plain prose is not JSON and must pass through byte-identical.
func TestMinify_ProseTextUnchanged(t *testing.T) {
	t.Parallel()

	raw := envelopeWithText(t, "3 projects found:\n  - alpha\n  - beta\n  - gamma")

	assert.Equal(t, raw, minifyToolResult(raw))
}

// Text that merely starts with a brace but is not valid JSON must not be
// rewritten (never corrupt what we do not understand).
func TestMinify_InvalidJSONLikeTextUnchanged(t *testing.T) {
	t.Parallel()

	raw := envelopeWithText(t, "{this is a Go struct literal, not JSON}")

	assert.Equal(t, raw, minifyToolResult(raw))
}

// json.Compact is byte-lossless: 64-bit integers survive exactly.
func TestMinify_PreservesBigIntegers(t *testing.T) {
	t.Parallel()

	bigID := "9007199254740993" // 2^53 + 1: corrupted by any float64 round trip
	raw := envelopeWithText(t, "{\n  \"id\": "+bigID+"\n}")

	got := minifyToolResult(raw)

	assert.Equal(t, `{"id":`+bigID+`}`, textOf(t, got))
}

// structuredContent that duplicates the text block is dropped; the spec has
// servers mirror the same JSON in both, billing it twice.
func TestMinify_DropsDuplicateStructuredContent(t *testing.T) {
	t.Parallel()

	payload := map[string]interface{}{"count": 3, "ok": true}
	payloadJSON, _ := json.Marshal(payload)
	raw, err := json.Marshal(map[string]interface{}{
		"content":           []map[string]string{{"type": "text", "text": string(payloadJSON)}},
		"structuredContent": payload,
	})
	require.NoError(t, err)

	got := minifyToolResult(raw)

	assert.NotContains(t, string(got), "structuredContent")
	assert.Contains(t, textOf(t, got), `"count":3`)
}

// structuredContent that differs from the text must be kept.
func TestMinify_KeepsDistinctStructuredContent(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(map[string]interface{}{
		"content":           []map[string]string{{"type": "text", "text": "summary: 3 items"}},
		"structuredContent": map[string]interface{}{"count": 3},
	})
	require.NoError(t, err)

	got := minifyToolResult(raw)

	assert.Contains(t, string(got), "structuredContent")
}

// Unparseable payloads pass through untouched.
func TestMinify_UnparseablePassesThrough(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage("not json at all")

	assert.Equal(t, raw, minifyToolResult(raw))
}

// Sibling fields in blocks and the envelope survive minification exactly.
func TestMinify_PreservesEnvelopeAndBlockFields(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"content":[{"type":"text","text":"{ \"a\": 1 }","annotations":{"audience":["user"],"bigRef":9007199254740993}}],"_meta":{"trace":"abc"},"isError":false}`)

	got := minifyToolResult(raw)

	assert.Contains(t, string(got), `"bigRef":9007199254740993`)
	assert.Contains(t, string(got), `"trace":"abc"`)
	assert.Contains(t, string(got), `"isError":false`)
	assert.Equal(t, `{"a":1}`, textOf(t, got))
}

func BenchmarkMinify_ProsePassThrough(b *testing.B) {
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": strings.Repeat("plain prose result ", 200)}},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		minifyToolResult(raw)
	}
}

func BenchmarkMinify_PrettyJSON(b *testing.B) {
	inner, _ := json.MarshalIndent(map[string]interface{}{"items": []int{1, 2, 3, 4, 5}, "nested": map[string]string{"k": strings.Repeat("v", 500)}}, "", "  ")
	raw, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": string(inner)}},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		minifyToolResult(raw)
	}
}
