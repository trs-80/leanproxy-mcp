package mcp

import "encoding/json"

// TruncationStat accumulates result-cap activity for one upstream tool.
// Byte counts are the serialized result payload as sent to the client, so
// BytesBefore-BytesAfter is the wire savings the cap actually delivered.
type TruncationStat struct {
	TruncatedCalls int64 `json:"truncated_calls"`
	BytesBefore    int64 `json:"bytes_before"`
	BytesAfter     int64 `json:"bytes_after"`
}

// applyResponseCap resolves the effective cap for server/tool and enforces it
// on result, recording stats and logging when the result is actually cut.
// Every dispatch path that returns a tool result (direct tools/call,
// invoke_tool, and their cache hits) must go through here — cap bugs
// historically came from one path missing what another got.
func (h *Handler) applyResponseCap(serverName, toolName string, result json.RawMessage, explicitCap int) json.RawMessage {
	capVal := h.responseCapFor(serverName, toolName, explicitCap)
	if capVal <= 0 {
		return result
	}
	trimmed, cut := truncateToolResultTracked(result, capVal, explicitCap > 0)
	if !cut {
		return result
	}

	key := serverName + "/" + toolName
	h.truncMu.Lock()
	if h.truncStats == nil {
		h.truncStats = make(map[string]TruncationStat)
	}
	s := h.truncStats[key]
	s.TruncatedCalls++
	s.BytesBefore += int64(len(result))
	s.BytesAfter += int64(len(trimmed))
	h.truncStats[key] = s
	h.truncMu.Unlock()

	h.logger.Info("result truncated",
		"tool", key,
		"cap", capVal,
		"bytes_before", len(result),
		"bytes_after", len(trimmed))
	return trimmed
}

// TruncationStats returns a copy of the per-tool truncation counters, keyed
// "server/tool". Tools whose results were never cut have no entry.
func (h *Handler) TruncationStats() map[string]TruncationStat {
	h.truncMu.Lock()
	defer h.truncMu.Unlock()
	out := make(map[string]TruncationStat, len(h.truncStats))
	for k, v := range h.truncStats {
		out[k] = v
	}
	return out
}
