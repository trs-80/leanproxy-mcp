package pool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
)

// oversizedLineChild is a shell child that answers its first request with a
// single JSON-RPC line whose result is roughly n bytes long, then keeps
// running (it blocks on a second read) so that only the proxy can end it.
func oversizedLineChild(n int) *migrate.StdioConfig {
	script := fmt.Sprintf(
		`read l; printf '{"jsonrpc":"2.0","id":1,"result":"'; head -c %d /dev/zero | tr '\0' a; printf '"}\n'; read l2`, n)
	return &migrate.StdioConfig{Command: "sh", Args: []string{"-c", script}}
}

// TestOversizedLineFailsFastAndMarksServerError: a child that emits a stdout
// line larger than the response cap must make the in-flight request fail
// promptly (not after the request timeout) and must leave the server in the
// error state so crash recovery restarts it, instead of silently killing the
// reader goroutine while the process stays alive and "healthy".
func TestOversizedLineFailsFastAndMarksServerError(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := NewStdioPool(5, 0, logger)
	defer p.Close()
	ctx := context.Background()

	const cap = 64 * 1024
	cfg := &migrate.ServerConfig{
		Name:      "oversized",
		Transport: "stdio",
		Stdio:     oversizedLineChild(2 * cap),
	}
	cfg.Stdio.MaxResponseBytes = cap
	if err := p.StartServer(ctx, cfg); err != nil {
		t.Fatalf("StartServer: %v", err)
	}

	const requestTimeout = 5 * time.Second
	start := time.Now()
	resp, err := p.SendRequestToServer(ctx, "oversized", "tools/list", nil, requestTimeout)
	elapsed := time.Since(start)
	if err == nil && (resp == nil || resp.Error == nil) {
		t.Fatalf("expected an error for an oversized response line, got %+v", resp)
	}
	if elapsed >= requestTimeout {
		t.Fatalf("request took %v: it timed out instead of failing fast", elapsed)
	}

	// waitForExit runs asynchronously once the child has been killed; poll
	// briefly, staying under the restart backoff so we observe the post-crash
	// state rather than a respawned child.
	deadline := time.Now().Add(500 * time.Millisecond)
	var state ServerState
	for time.Now().Before(deadline) {
		state, _ = p.GetServerState("oversized")
		if state == StateError || state == StateStopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state != StateError && state != StateStopped {
		t.Fatalf("server reported %v after an oversized line, want error", state)
	}
	if _, err := p.GetServer("oversized"); err == nil {
		t.Fatal("GetServer returned the wedged server as healthy")
	}
}

// TestMaxResponseBytesIsHonoured: the configured cap, not the built-in
// default, decides what counts as oversized — a line under the cap is
// delivered, a line over it fails.
func TestMaxResponseBytesIsHonoured(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := NewStdioPool(5, 0, logger)
	defer p.Close()
	ctx := context.Background()

	const cap = 16 * 1024
	cases := []struct {
		name    string
		payload int
		wantErr bool
	}{
		{"under-cap", cap / 2, false},
		{"over-cap", 2 * cap, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &migrate.ServerConfig{
				Name:      tc.name,
				Transport: "stdio",
				Stdio:     oversizedLineChild(tc.payload),
			}
			cfg.Stdio.MaxResponseBytes = cap
			if err := p.StartServer(ctx, cfg); err != nil {
				t.Fatalf("StartServer: %v", err)
			}
			resp, err := p.SendRequestToServer(ctx, tc.name, "tools/list", nil, 5*time.Second)
			gotErr := err != nil || resp == nil || resp.Error != nil
			if gotErr != tc.wantErr {
				t.Fatalf("payload %d bytes with cap %d: err=%v resp=%+v, wantErr=%v", tc.payload, cap, err, resp, tc.wantErr)
			}
			if !tc.wantErr && len(resp.Result) < tc.payload {
				t.Fatalf("result truncated: got %d bytes, want >= %d", len(resp.Result), tc.payload)
			}
		})
	}
}
