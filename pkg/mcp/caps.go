package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// minResponseChars is the smallest cap accepted for max_response_chars; below
// this a truncated result is unlikely to be usable and the marker overhead
// dominates.
const minResponseChars = 200

// truncationMarkerSlack: results over the cap by less than this are passed
// through untouched, since the truncation marker is itself ~90 chars.
const truncationMarkerSlack = 100

// maxExplicitCap bounds explicit caps before float64->int conversion:
// int(huge float64) is implementation-defined (MinInt64 on amd64, which
// responseCapFor would read as "no explicit value", silently re-applying
// the configured cap in a retry loop).
const maxExplicitCap = math.MaxInt32

// SetDefaultMaxResponseChars sets a server-side default cap applied to every
// invoke_tool result that does not carry an explicit max_response_chars.
// Zero (the default) means unlimited.
func (h *Handler) SetDefaultMaxResponseChars(n int) {
	if n > 0 && n < minResponseChars {
		n = minResponseChars
	}
	h.defaultMaxResponseChars = n
}

// SetToolMaxResponseChars sets a per-tool result cap (chars). Values below
// minResponseChars are raised to it; zero removes the cap.
func (h *Handler) SetToolMaxResponseChars(serverName, toolName string, n int) {
	if h.toolResponseCaps == nil {
		h.toolResponseCaps = make(map[string]int)
	}
	key := serverName + "/" + toolName
	if n <= 0 {
		delete(h.toolResponseCaps, key)
		return
	}
	if n < minResponseChars {
		n = minResponseChars
	}
	h.toolResponseCaps[key] = n
}

// responseCapFor resolves the effective result cap: explicit argument, then
// per-tool config, then the global default. Zero means unlimited.
func (h *Handler) responseCapFor(serverName, toolName string, explicit int) int {
	if explicit > 0 {
		if explicit < minResponseChars {
			return minResponseChars
		}
		return explicit
	}
	if n, ok := h.toolResponseCaps[serverName+"/"+toolName]; ok {
		return n
	}
	return h.defaultMaxResponseChars
}

// parseCapValue reads a positive cap from a JSON number or a numeric string
// ("2000" is a common model slip), clamped to maxExplicitCap. 0 means
// absent or unusable.
func parseCapValue(raw json.RawMessage) int {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0
		}
		n = json.Number(strings.TrimSpace(s))
	}
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return 0
	}
	if f >= maxExplicitCap {
		return maxExplicitCap
	}
	return int(f)
}

// upstreamDeclaresParam reports whether the cached schema for server/tool
// declares the property, meaning the parameter belongs to the tool itself
// and must not be consumed by the proxy.
func (h *Handler) upstreamDeclaresParam(serverName, toolName, param string) bool {
	schema := h.lookupToolSchema(serverName, toolName)
	if schema == nil {
		return false
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return false
	}
	_, ok := s.Properties[param]
	return ok
}

// extractResponseCap pops max_response_chars out of a tool-argument object.
// Sibling arguments keep their exact bytes (map[string]json.RawMessage —
// round-tripping through interface{} turned 64-bit IDs into float64 and
// corrupted them). The key passes through untouched when the upstream tool
// legitimately declares it. The bytes.Contains guard keeps the common case
// (no cap argument) free of a full JSON parse on the hot path.
func (h *Handler) extractResponseCap(serverName, toolName string, arguments json.RawMessage) (json.RawMessage, int) {
	if len(arguments) == 0 || !bytes.Contains(arguments, []byte(`"max_response_chars"`)) {
		return arguments, 0
	}
	if h.upstreamDeclaresParam(serverName, toolName, "max_response_chars") {
		return arguments, 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &m); err != nil {
		return arguments, 0
	}
	raw, ok := m["max_response_chars"]
	if !ok {
		return arguments, 0
	}
	capVal := parseCapValue(raw)
	delete(m, "max_response_chars")
	if b, err := json.Marshal(m); err == nil {
		arguments = b
	}
	return arguments, capVal
}

// truncateToolResult enforces a total character budget across the text blocks
// of an MCP tools/call result. Non-text blocks and unparseable results pass
// through untouched (never corrupt what we do not understand). A marker noting
// the cut is appended so the model knows the output is partial and how to get
// the rest.
func truncateToolResult(raw json.RawMessage, maxChars int, explicitCap bool) json.RawMessage {
	out, _ := truncateToolResultTracked(raw, maxChars, explicitCap)
	return out
}

// truncateToolResultTracked is truncateToolResult plus a flag reporting
// whether the result was actually cut, so callers can count truncations
// without comparing payloads.
func truncateToolResultTracked(raw json.RawMessage, maxChars int, explicitCap bool) (json.RawMessage, bool) {
	// Fast path: decoded text can never exceed its JSON encoding (escapes only
	// shrink on decode) and structuredContent is a subslice of raw, so a
	// payload no larger than cap+slack cannot need truncation. This skips the
	// full parse for the common under-cap result on the hot path.
	if len(raw) <= maxChars+truncationMarkerSlack {
		return raw, false
	}

	// The envelope is kept as raw fields so truncation never drops top-level
	// members it does not model (structuredContent, _meta, ...): rebuilding
	// only {content, isError} silently violated the result schema for tools
	// with an outputSchema.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return raw, false
	}
	contentKey := "content"
	if _, ok := envelope[contentKey]; !ok {
		// The spec mandates lowercase, but a nonconforming "Content" must
		// degrade to a pass-through of the cap, not a bypass of it (the old
		// struct-tag decode matched case-insensitively).
		for k := range envelope {
			if strings.EqualFold(k, "content") {
				contentKey = k
				break
			}
		}
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(envelope[contentKey], &blocks); err != nil || len(blocks) == 0 {
		return raw, false
	}

	total := 0
	for _, block := range blocks {
		if text, ok := block["text"].(string); ok {
			total += len(text)
		}
	}
	// structuredContent counts against the cap too: it is the machine copy
	// of the same result, and leaving it unbounded made the cap a no-op for
	// exactly the outputSchema tools the envelope rewrite preserved it for.
	structuredLen := len(envelope["structuredContent"])

	// Truncating only pays when it saves more than the marker costs.
	textOver := total > maxChars+truncationMarkerSlack
	structuredOver := structuredLen > maxChars+truncationMarkerSlack
	if !textOver && !structuredOver {
		return raw, false
	}

	kept := make([]map[string]interface{}, 0, len(blocks))
	if textOver {
		budget := maxChars
		for _, block := range blocks {
			text, ok := block["text"].(string)
			if !ok {
				// Non-text blocks (images, resources) are never truncated and
				// must survive even after the text budget is spent.
				kept = append(kept, block)
				continue
			}
			if budget <= 0 {
				continue
			}
			if len(text) > budget {
				cut := budget
				// Back up to a rune boundary: a mid-rune byte cut marshals
				// as U+FFFD mojibake right where the model reads the marker.
				for cut > 0 && !utf8.RuneStart(text[cut]) {
					cut--
				}
				nb := make(map[string]interface{}, len(block))
				for k, v := range block {
					nb[k] = v
				}
				nb["text"] = text[:cut]
				kept = append(kept, nb)
				budget = 0
				continue
			}
			budget -= len(text)
			kept = append(kept, block)
		}
	} else {
		kept = append(kept, blocks...)
	}

	var notes []string
	if textOver {
		notes = append(notes, fmt.Sprintf("truncated, %d of %d chars shown", maxChars, total))
	}
	if structuredOver {
		delete(envelope, "structuredContent")
		notes = append(notes, fmt.Sprintf("structuredContent (%d chars) omitted", structuredLen))
	}
	// Advice the model can actually follow: an explicit argument can be
	// raised; a config cap is overridden by passing the argument. "omit"
	// was a guaranteed no-op for config caps (re-resolves to the same cap).
	advice := "pass max_response_chars for more"
	if explicitCap {
		advice = "raise max_response_chars for more"
	}
	marker := fmt.Sprintf("\n[leanproxy: %s; %s]", strings.Join(notes, "; "), advice)
	kept = append(kept, map[string]interface{}{"type": "text", "text": marker})

	newContent, err := json.Marshal(kept)
	if err != nil {
		return raw, false
	}
	envelope[contentKey] = newContent
	trimmed, err := json.Marshal(envelope)
	if err != nil {
		return raw, false
	}
	return trimmed, true
}
