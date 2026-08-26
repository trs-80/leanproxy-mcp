package mcp

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// usageSaveInterval throttles usage-file writes; a crash loses at most this
// much recency, which only delays a demotion or un-demotion by one window.
const usageSaveInterval = 5 * time.Second

// usageRecord tracks when a tool was first offered to a client and last
// actually invoked, keyed "server/tool".
type usageRecord struct {
	FirstSeen int64 `json:"first_seen"`
	LastUsed  int64 `json:"last_used,omitempty"`
}

// usageTracker persists tool-usage recency across sessions so adaptive stubs
// have history to act on. All methods are safe for concurrent use.
type usageTracker struct {
	mu       sync.Mutex
	path     string
	logger   *slog.Logger
	records  map[string]usageRecord
	lastSave time.Time
	dirty    bool
}

func loadUsageTracker(path string, logger *slog.Logger) *usageTracker {
	if logger == nil {
		logger = slog.Default()
	}
	t := &usageTracker{path: path, logger: logger, records: make(map[string]usageRecord)}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("adaptive stubs: cannot read usage file, starting fresh", "path", path, "error", err)
		}
		return t
	}
	if err := json.Unmarshal(data, &t.records); err != nil {
		logger.Warn("adaptive stubs: unparseable usage file, starting fresh", "path", path, "error", err)
		t.records = make(map[string]usageRecord)
	}
	return t
}

// touchSeen records that a tool was offered to a client (first time only).
func (t *usageTracker) touchSeen(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.records[key]; ok {
		return
	}
	t.records[key] = usageRecord{FirstSeen: now.Unix()}
	t.dirty = true
}

// touchUsed records an invocation.
func (t *usageTracker) touchUsed(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := t.records[key]
	if rec.FirstSeen == 0 {
		rec.FirstSeen = now.Unix()
	}
	rec.LastUsed = now.Unix()
	t.records[key] = rec
	t.dirty = true
}

func (t *usageTracker) record(key string) (usageRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[key]
	return rec, ok
}

// maybeSave writes the usage file atomically (temp + rename), throttled
// unless forced.
func (t *usageTracker) maybeSave(force bool) {
	t.mu.Lock()
	if !t.dirty || (!force && time.Since(t.lastSave) < usageSaveInterval) {
		t.mu.Unlock()
		return
	}
	data, err := json.Marshal(t.records)
	t.dirty = false
	t.lastSave = time.Now()
	path := t.path
	t.mu.Unlock()
	if err != nil {
		t.logger.Warn("adaptive stubs: marshal usage failed", "error", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".toolusage-*")
	if err != nil {
		t.logger.Warn("adaptive stubs: write usage file failed", "path", path, "error", err)
		return
	}
	if _, err := tmp.Write(data); err == nil {
		err = tmp.Close()
		if err == nil {
			err = os.Rename(tmp.Name(), path)
		}
	} else {
		tmp.Close()
	}
	if err != nil {
		os.Remove(tmp.Name())
		t.logger.Warn("adaptive stubs: write usage file failed", "path", path, "error", err)
	}
}

// EnableUsageTracking loads (or creates) the persistent usage file adaptive
// stubs decide from. Must be called before serving when any server sets
// adaptive_stub_after.
func (h *Handler) EnableUsageTracking(path string) {
	h.usage = loadUsageTracker(path, h.logger)
}

// SetAdaptiveStubWindow enables usage-adaptive stubs for a server: a tool not
// invoked within the window (counted from first sight for never-used tools)
// renders as a name-only stub in lazy tools/list. The tool stays fully
// callable and searchable — search_tools and get_tool_schema keep the full
// data — and one invocation restores its full stub on the next tools/list.
// Zero or negative removes the entry.
func (h *Handler) SetAdaptiveStubWindow(serverName string, window time.Duration) {
	if h.adaptiveWindows == nil {
		h.adaptiveWindows = make(map[string]time.Duration)
	}
	if window <= 0 {
		delete(h.adaptiveWindows, serverName)
		return
	}
	h.adaptiveWindows[serverName] = window
}

// recordToolUse feeds the adaptive-stub tracker; called on every dispatch
// (cache hits included — a replayed call is still usage).
func (h *Handler) recordToolUse(serverName, toolName string) {
	if h.usage == nil {
		return
	}
	h.usage.touchUsed(serverName+"/"+toolName, time.Now())
	h.usage.maybeSave(false)
}

// stubDemoted reports whether a tool's stub should render name-only. Tools
// with no usage record are never demoted (they may be brand new): they get a
// FirstSeen stamp instead and a full window before demotion.
func (h *Handler) stubDemoted(serverName, toolName string, now time.Time) bool {
	window, ok := h.adaptiveWindows[serverName]
	if !ok || h.usage == nil {
		return false
	}
	key := serverName + "/" + toolName
	rec, ok := h.usage.record(key)
	if !ok {
		h.usage.touchSeen(key, now)
		return false
	}
	ref := rec.LastUsed
	if ref == 0 {
		ref = rec.FirstSeen
	}
	return now.Unix()-ref > int64(window/time.Second)
}
