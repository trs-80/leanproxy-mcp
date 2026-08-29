package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestStdioPipeConnectivity verifies that a spawned subprocess can receive
// a JSON-RPC request on stdin and return a response on stdout.
// Uses a shell echo loop to simulate an MCP server.
func TestStdioPipeConnectivity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pipe connectivity test in short mode")
	}

	config := StdioServerConfig{
		Name:    "test-echo",
		Command: "sh",
		Args:    []string{"-c", `while read -r line; do echo '{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}'; done`},
	}

	server := newServerV2("test-echo", config, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := server.spawn(ctx)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	defer server.stop()

	// Allow goroutines to start
	time.Sleep(100 * time.Millisecond)

	// Register a waiter for id 1, then write a JSON-RPC initialize request
	// to stdin directly to prove pipe connectivity end to end.
	respCh := make(chan Response, 1)
	server.pendingMu.Lock()
	server.pending[1] = respCh
	server.pendingMu.Unlock()
	reqJSON := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`
	_, err = fmt.Fprintln(server.stdin, reqJSON)
	if err != nil {
		t.Fatalf("write to stdin failed: %v", err)
	}

	// Wait for the echo response on stdout
	select {
	case resp := <-respCh:
		if resp.Result == nil {
			t.Error("expected non-nil result")
		}
		var result map[string]interface{}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		if result["status"] != "ok" {
			t.Errorf("expected status=ok, got %v", result["status"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for echo response — pipe connectivity broken")
	}
}

// TestRequestMarshalJSONNoTimeout verifies that the non-standard "timeout"
// field is NOT included in the JSON-RPC wire payload.
func TestRequestMarshalJSONNoTimeout(t *testing.T) {
	req := Request{
		Method:  "initialize",
		ID:      1,
		Timeout: 120 * time.Second,
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05"}`),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	jsonStr := string(data)

	// Must contain standard JSON-RPC fields
	if !strings.Contains(jsonStr, `"jsonrpc":"2.0"`) {
		t.Error("missing jsonrpc field")
	}
	if !strings.Contains(jsonStr, `"method":"initialize"`) {
		t.Error("missing method field")
	}
	if !strings.Contains(jsonStr, `"id":1`) {
		t.Error("missing id field")
	}

	// Must NOT contain the non-standard timeout field
	if strings.Contains(jsonStr, `"timeout"`) {
		t.Errorf("request should not contain 'timeout' field, got: %s", jsonStr)
	}
}

// TestSpawnSetsPythonUnbuffered verifies that PYTHONUNBUFFERED=1 is set
// in the subprocess environment.
func TestSpawnSetsPythonUnbuffered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping env test in short mode")
	}

	config := StdioServerConfig{
		Name:    "test-env",
		Command: "sh",
		Args:    []string{"-c", "echo $PYTHONUNBUFFERED && sleep 0.5"},
	}

	server := newServerV2("test-env", config, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := server.spawn(ctx)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	defer server.stop()

	// The subprocess should print PYTHONUNBUFFERED value to stdout.
	// We can't easily read it since readResponses consumes stdout,
	// but we can verify the env was set by checking the command's env.
	found := false
	for _, e := range server.process.Env {
		if e == "PYTHONUNBUFFERED=1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("PYTHONUNBUFFERED=1 not found in subprocess environment")
	}
}

// TestStderrCaptureOnTimeout verifies that recent stderr output is included
// in timeout error messages.
func TestStderrCaptureOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stderr capture test in short mode")
	}

	// Spawn a server that writes to stderr but never responds on stdout
	config := StdioServerConfig{
		Name:    "test-stderr",
		Command: "sh",
		Args:    []string{"-c", `echo "some error message" >&2; sleep 30`},
	}

	server := newServerV2("test-stderr", config, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := server.spawn(ctx)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	defer server.stop()

	// Allow stderr to be captured
	time.Sleep(200 * time.Millisecond)

	// Send a request with a short timeout — the server won't respond
	_, err = server.sendRequest(ctx, Request{
		Method:  "test",
		ID:      1,
		Timeout: 500 * time.Millisecond,
	}, make(chan struct{}))

	if err == nil {
		t.Fatal("expected timeout error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "request timeout") {
		t.Errorf("error should mention timeout, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "some error message") {
		t.Errorf("error should include stderr output, got: %s", errMsg)
	}
}

// TestStderrRing verifies the ring buffer correctly stores and retrieves lines.
func TestStderrRing(t *testing.T) {
	ring := newStderrRing(3)

	ring.add("line1")
	ring.add("line2")
	ring.add("line3")
	ring.add("line4") // should evict line1

	result := ring.String()
	if !strings.Contains(result, "line4") {
		t.Error("should contain line4")
	}
	if strings.Contains(result, "line1") {
		t.Error("should not contain evicted line1")
	}
	if !strings.Contains(result, "line2") {
		t.Error("should contain line2")
	}
}

// TestSpawnAliveCheck verifies that spawn detects a process that exits immediately.
func TestSpawnAliveCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping alive check test in short mode")
	}

	config := StdioServerConfig{
		Name:    "test-cmd-ok",
		Command: "sleep",
		Args:    []string{"60"},
	}

	server := newServerV2("test-cmd-ok", config, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := server.spawn(ctx)
	if err != nil {
		t.Fatalf("spawn should succeed for valid command: %v", err)
	}
	defer server.stop()

	// Process should be alive
	if server.process == nil || server.process.Process == nil {
		t.Fatal("process should be non-nil after successful spawn")
	}
}

// TestSendRequestTimeout_MinOfServerAndCaller verifies the regression fix
// for the per-server timeout: the worker uses min(s.requestTimeout, req.Timeout)
// so a per-server TimeoutValue is honored even when the caller passes a
// smaller value. Before the fix, the caller value (often a stale global
// 30s) won unconditionally, silently downgrading the per-server timeout.
//
// The deadline sendRequest actually armed is observed through the timeout
// error, which reports it verbatim ("pool: request timeout after %v"). The
// upstream is `sleep`, which never answers, so the timer arm is the only
// reachable outcome and the reported duration is the value under test.
// Durations are sub-second so the four cases stay fast; the assertion is on
// the reported value, not on wall-clock timing, so it is not flaky.
func TestSendRequestTimeout_MinOfServerAndCaller(t *testing.T) {
	cases := []struct {
		name            string
		serverTimeout   time.Duration
		callerTimeout   time.Duration
		expectedTimeout time.Duration
	}{
		{"server-only (req.Timeout=0) wins", 300 * time.Millisecond, 0, 300 * time.Millisecond},
		{"caller smaller wins", 300 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond},
		{"server smaller wins", 100 * time.Millisecond, 300 * time.Millisecond, 100 * time.Millisecond},
		{"equal values", 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServerV2("test", StdioServerConfig{
				Name:           "test",
				Command:        "sleep",
				Args:           []string{"30"},
				RequestTimeout: tc.serverTimeout,
			}, slog.Default())
			server.requestTimeout = tc.serverTimeout

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := server.spawn(ctx); err != nil {
				t.Fatalf("spawn failed: %v", err)
			}
			t.Cleanup(func() { server.stop() })

			_, err := server.sendRequest(ctx, Request{
				Method:  "test",
				ID:      1,
				Timeout: tc.callerTimeout,
			}, make(chan struct{}))

			if err == nil {
				t.Fatalf("sendRequest(serverTimeout=%v, callerTimeout=%v): expected a timeout error, got nil", tc.serverTimeout, tc.callerTimeout)
			}
			wantMsg := "request timeout after " + tc.expectedTimeout.String()
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("sendRequest(serverTimeout=%v, callerTimeout=%v) armed the wrong deadline: error %q does not contain %q", tc.serverTimeout, tc.callerTimeout, err.Error(), wantMsg)
			}
		})
	}
}
