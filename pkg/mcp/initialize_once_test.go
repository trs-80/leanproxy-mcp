package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/errors"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/pool"
)

// strictInitPool models what a spec-compliant MCP server actually does: it
// accepts exactly one `initialize` per process and rejects the second.
//
// The shared mockPool cannot express this — its IsServerMCPInitialized always
// returns false and its MarkServerMCPInitialized is a no-op, so every test
// using it re-initializes freely and no test could ever have caught a
// duplicate handshake. That gap is why this bug reached a real session.
//
// The error string is copied from the failure github-mcp-server produced
// through leanproxy:
//
//	MCP error -32000: server initialization failed: server returned error:
//	jsonrpc: error 0: duplicate "initialize" received
type strictInitPool struct {
	mu sync.Mutex

	// initializeRequests counts `initialize` requests that reached the
	// server, per server name — the number this test exists to pin at 1.
	initializeRequests map[string]int
	// serverSaysInitialized is the SERVER's view: set by the first
	// successful handshake, cleared only by a respawn.
	serverSaysInitialized map[string]bool
	// poolMarked is the POOL's view, what IsServerMCPInitialized reports.
	poolMarked map[string]bool
	tools      []Tool
}

func newStrictInitPool(tools []Tool) *strictInitPool {
	return &strictInitPool{
		initializeRequests:    map[string]int{},
		serverSaysInitialized: map[string]bool{},
		poolMarked:            map[string]bool{},
		tools:                 tools,
	}
}

func (p *strictInitPool) SendRequestToServer(_ context.Context, name, method string, _ json.RawMessage, _ time.Duration) (*pool.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch method {
	case MethodInitialize:
		p.initializeRequests[name]++
		if p.serverSaysInitialized[name] {
			return &pool.Response{Error: &errors.JSONRPCError{
				Code:    0,
				Message: `jsonrpc: error 0: duplicate "initialize" received`,
			}}, nil
		}
		p.serverSaysInitialized[name] = true
		return &pool.Response{Result: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)}, nil

	case MethodToolsList:
		if !p.serverSaysInitialized[name] {
			return &pool.Response{Error: &errors.JSONRPCError{
				Code:    0,
				Message: "tools/list before initialize",
			}}, nil
		}
		body, _ := json.Marshal(map[string]any{"tools": p.tools})
		return &pool.Response{Result: body}, nil

	default:
		return &pool.Response{Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)}, nil
	}
}

func (p *strictInitPool) SendRequestToServerWithID(ctx context.Context, name, method string, params json.RawMessage, timeout time.Duration, _ int) (*pool.Response, error) {
	return p.SendRequestToServer(ctx, name, method, params, timeout)
}

func (p *strictInitPool) SendServerNotification(context.Context, string, string, map[string]interface{}) error {
	return nil
}

func (p *strictInitPool) ListServers() []string { return []string{"github"} }

func (p *strictInitPool) GetServerState(name string) (pool.ServerState, error) {
	if name != "github" {
		return "", fmt.Errorf("server not found")
	}
	return pool.StateRunning, nil
}

// RestartServer models a respawn: the server forgets its handshake, so a fresh
// initialize is both allowed and required.
func (p *strictInitPool) RestartServer(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serverSaysInitialized[name] = false
	p.poolMarked[name] = false
	return nil
}

func (p *strictInitPool) IsServerMCPInitialized(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poolMarked[name]
}

func (p *strictInitPool) MarkServerMCPInitialized(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poolMarked[name] = true
}

func (p *strictInitPool) Close() error { return nil }

func (p *strictInitPool) initCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initializeRequests[name]
}

func githubFixtureTools() []Tool {
	return []Tool{{
		Name:        "list_pull_requests",
		Description: "List pull requests in a GitHub repository.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"}},"required":["owner"]}`),
	}}
}

// TestInitializeServer_IsIdempotentAcrossCallSites reproduces the live failure.
//
// Four places call initializeServer — toolcall.go:75, toolcall.go:245,
// toolcache.go:151 and discovery.go:297 — but only the two in toolcall.go
// consult IsServerMCPInitialized/MarkServerMCPInitialized first. So the tool
// cache warm-up performs a real handshake and never records it, and the next
// tool call sees an unmarked server and hands the upstream a second
// initialize. A tolerant server ignores that; a spec-compliant one rejects it,
// and every call through leanproxy to github-mcp-server failed with
// `duplicate "initialize" received`.
//
// The fix belongs inside initializeServer, not at each caller: a guard added
// per-call-site is one refactor away from being missed again, which is exactly
// how this happened.
func TestInitializeServer_IsIdempotentAcrossCallSites(t *testing.T) {
	p := newStrictInitPool(githubFixtureTools())
	h := NewHandler(p, slog.New(slog.DiscardHandler))

	// The unguarded warm-up path.
	if err := h.initializeServer(context.Background(), "github"); err != nil {
		t.Fatalf("first initialize failed: %v", err)
	}
	// Any later path, guarded or not.
	if err := h.initializeServer(context.Background(), "github"); err != nil {
		t.Fatalf("second initializeServer must be a no-op, got error: %v", err)
	}

	if got := p.initCount("github"); got != 1 {
		t.Errorf("upstream received %d initialize requests, want exactly 1 "+
			"(a second one is what a spec-compliant server rejects)", got)
	}
}

// TestInitializeServer_ReinitializesAfterRespawn is the other half of the
// contract: skipping the handshake is only correct while the process that
// completed it is still alive. StdioServerV2.spawn clears mcpInitialized on
// every new generation (pkg/pool/server.go), so after a restart the guard must
// report false and a fresh initialize must go out — otherwise a crashed and
// restarted server would serve every subsequent call uninitialized.
func TestInitializeServer_ReinitializesAfterRespawn(t *testing.T) {
	p := newStrictInitPool(githubFixtureTools())
	h := NewHandler(p, slog.New(slog.DiscardHandler))

	if err := h.initializeServer(context.Background(), "github"); err != nil {
		t.Fatalf("first initialize failed: %v", err)
	}
	if err := p.RestartServer(context.Background(), "github"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := h.initializeServer(context.Background(), "github"); err != nil {
		t.Fatalf("initialize after respawn failed: %v", err)
	}

	if got := p.initCount("github"); got != 2 {
		t.Errorf("upstream received %d initialize requests, want 2 "+
			"(one per process generation)", got)
	}
}
