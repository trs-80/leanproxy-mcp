package mcp

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/pool"
)

// capturingHandler records the level and message of every log record so a test
// can assert on how something was reported, not just that it happened.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingHandler) WithGroup(string) slog.Handler      { return c }

// atLevel returns the messages logged at exactly the given level.
func (c *capturingHandler) atLevel(level slog.Level) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.records {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

// startupStatePool reports a server as still starting up, the state an HTTP
// transport occupies for its first few hundred milliseconds.
type startupStatePool struct {
	*mockPool
	state pool.ServerState
}

func (p *startupStatePool) GetServerState(string) (pool.ServerState, error) {
	return p.state, nil
}

// TestToolCacheRefresh_DoesNotWarnWhileAServerIsStillStarting pins how a
// transient state is reported.
//
// The cache refresh treats anything outside idle/running/busy as "not running"
// and logs a WARN before restarting it. `starting` is not a fault — an HTTP
// transport sits there for a few hundred milliseconds on every cold start — so
// a real Bob session logged, three times in ninety seconds:
//
//	WARN "server not running, attempting restart for cache refresh"
//	     name=context7 state=starting
//
// Nothing was wrong, context7 worked, and the warning is exactly the kind of
// thing that sends someone debugging a healthy system. The restart still
// happens; only the reporting level changes, and only for states that are
// transient by definition.
func TestToolCacheRefresh_DoesNotWarnWhileAServerIsStillStarting(t *testing.T) {
	for _, state := range []pool.ServerState{pool.StateStarting, pool.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			cap := &capturingHandler{}
			mp := newMockPool()
			mp.servers["context7"] = string(state)

			h := NewHandler(&startupStatePool{mockPool: mp, state: state}, slog.New(cap))
			h.PopulateToolCache(context.Background())

			for _, msg := range cap.atLevel(slog.LevelWarn) {
				if strings.Contains(msg, "server not running") {
					t.Errorf("state %q is transient and must not be reported at WARN; got: %q", state, msg)
				}
			}
		})
	}
}

// TestToolCacheRefresh_StillWarnsForAGenuinelyBrokenServer is the other half:
// quieting `starting` must not quiet a server that really is down, or the
// change trades a false alarm for a silent failure.
func TestToolCacheRefresh_StillWarnsForAGenuinelyBrokenServer(t *testing.T) {
	for _, state := range []pool.ServerState{pool.StateError, pool.StateStopped, pool.StateDisconnected} {
		t.Run(string(state), func(t *testing.T) {
			cap := &capturingHandler{}
			mp := newMockPool()
			mp.servers["context7"] = string(state)

			h := NewHandler(&startupStatePool{mockPool: mp, state: state}, slog.New(cap))
			h.PopulateToolCache(context.Background())

			var found bool
			for _, msg := range cap.atLevel(slog.LevelWarn) {
				if strings.Contains(msg, "server not running") {
					found = true
				}
			}
			if !found {
				t.Errorf("state %q is a real fault and must still be reported at WARN", state)
			}
		})
	}
}
