package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
)

// SetMinifyResults toggles result minification (default on). Minification is
// lossless for JSON semantics: text blocks are compacted with json.Compact
// (whitespace only — never reorders keys or rewrites numbers) and
// structuredContent is dropped only when byte-equivalent to a text block.
func (h *Handler) SetMinifyResults(on bool) {
	h.minifyResults = on
}

// minifyToolResult shrinks a tools/call result without changing what it
// means: pretty-printed JSON inside text blocks is compacted, and a
// structuredContent that merely duplicates a text block is dropped (the spec
// has servers mirror the same JSON in both — billed twice on every turn).
// Anything that does not parse passes through untouched.
func minifyToolResult(raw json.RawMessage) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return raw
	}
	contentKey := "content"
	if _, ok := envelope[contentKey]; !ok {
		for k := range envelope {
			if strings.EqualFold(k, "content") {
				contentKey = k
				break
			}
		}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(envelope[contentKey], &blocks); err != nil || len(blocks) == 0 {
		return raw
	}

	structured := envelope["structuredContent"]
	compactStructured := compactJSON(structured)

	changed := false
	dropStructured := false
	for i, blockRaw := range blocks {
		// Raw-field decoding: sibling fields keep their exact bytes (an
		// interface{} round trip corrupts 64-bit integers).
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			continue
		}
		compacted := compactJSON(json.RawMessage(trimmed))
		if compacted == nil {
			continue
		}
		if compactStructured != nil && bytes.Equal(compacted, compactStructured) {
			dropStructured = true
		}
		if len(compacted) >= len(text) {
			continue
		}
		newText, err := json.Marshal(string(compacted))
		if err != nil {
			continue
		}
		block["text"] = newText
		newBlock, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks[i] = newBlock
		changed = true
	}

	if dropStructured {
		delete(envelope, "structuredContent")
		changed = true
	}
	if !changed {
		return raw
	}

	newContent, err := json.Marshal(blocks)
	if err != nil {
		return raw
	}
	envelope[contentKey] = newContent
	minified, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return minified
}

// compactJSON returns the whitespace-free form of valid JSON, nil otherwise.
// json.Compact is byte-lossless apart from insignificant whitespace.
func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}
	return buf.Bytes()
}
