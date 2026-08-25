package pool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
)

// fakeStreamableMCP is a minimal streamable-HTTP MCP endpoint: it answers the
// protocol initialize, accepts notifications, and records every tools/call by
// tool name so tests can assert what reached the upstream.
type fakeStreamableMCP struct {
	mu        sync.Mutex
	toolCalls []string
}

func (f *fakeStreamableMCP) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.toolCalls...)
}

func (f *fakeStreamableMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     interface{}     `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)

		if req.ID == nil {
			// Notification (e.g. notifications/initialized).
			w.WriteHeader(http.StatusAccepted)
			return
		}

		write := func(result interface{}) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  result,
			})
		}

		switch req.Method {
		case "initialize":
			write(map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo":      map[string]interface{}{"name": "fake", "version": "1.0"},
			})
		case "tools/list":
			write(map[string]interface{}{"tools": []interface{}{}})
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			f.toolCalls = append(f.toolCalls, p.Name)
			f.mu.Unlock()
			write(map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": "ok"}},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]interface{}{"code": -32601, "message": "method not found"},
			})
		}
	}
}

// The gateway handler sends an "initialize" request to every server before a
// tool call. On HTTP transports the real MCP handshake already happens inside
// ensureConnected, so the pool must answer initialize locally — forwarding it
// used to reach the upstream as a tool literally named "initialize" and fail
// on every cache refresh.
func TestHTTPPool_InitializeAnsweredLocally_NeverForwardedAsToolCall(t *testing.T) {
	fake := &fakeStreamableMCP{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := NewHTTPClientPool(nil)
	defer p.Close()
	err := p.StartServer(context.Background(), &migrate.ServerConfig{
		Name: "fake",
		HTTP: &migrate.HTTPConfig{URL: srv.URL},
	})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}

	resp, err := p.SendRequestToServerWithID(context.Background(), "fake", "initialize", nil, 5*time.Second, 1)
	if err != nil {
		t.Fatalf("initialize through pool: %v", err)
	}
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize must succeed locally, got %+v", resp)
	}

	for _, name := range fake.calls() {
		if name == "initialize" {
			t.Fatal("initialize was forwarded to the upstream as a tools/call")
		}
	}

	// A real tool call on the same connection still reaches the upstream.
	args, _ := json.Marshal(map[string]interface{}{"name": "echo", "arguments": map[string]interface{}{}})
	if _, err := p.SendRequestToServerWithID(context.Background(), "fake", "tools/call", args, 5*time.Second, 2); err != nil {
		t.Fatalf("tools/call through pool: %v", err)
	}
	calls := fake.calls()
	if len(calls) != 1 || calls[0] != "echo" {
		t.Fatalf("upstream tool calls = %v, want exactly [echo]", calls)
	}
}
