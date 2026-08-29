package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/errors"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/gateway"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/proxy"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/registry"
)

// leakyDSN mimics a transport error that embeds both a credentialed DSN and
// a token the built-in redactor recognizes.
const leakyDSN = "postgres://app:S3cretPassw0rd@db.internal:5432/app key=AKIAIOSFODNN7EXAMPLE"

var correlationIDRe = regexp.MustCompile(`\b[0-9a-f]{8}\b`)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestUpstreamErrorResponse(t *testing.T) {
	logs := captureLogs(t)
	err := fmt.Errorf("pool: dial %s: connection refused", leakyDSN)

	msg, cid := upstreamErrorResponse("test-server", "tools/call", err)

	if msg != upstreamErrorMessage+" (ref "+cid+")" {
		t.Fatalf("expected fixed message with correlation id, got %q", msg)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(cid) {
		t.Fatalf("expected 8 hex char correlation id, got %q", cid)
	}
	if strings.Contains(msg, "S3cretPassw0rd") {
		t.Fatalf("client message leaks secret: %q", msg)
	}
	out := logs.String()
	if !strings.Contains(out, cid) {
		t.Fatalf("server log missing correlation id %s: %s", cid, out)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("server log contains unredacted secret: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("server log missing real error detail: %s", out)
	}
}

func newLeakyPool() *mockPool {
	return &mockPool{
		sendRequestFunc: func(ctx context.Context, serverName string, req *proxy.JSONRPCRequest, timeout time.Duration) (*proxy.JSONRPCResponse, error) {
			return nil, fmt.Errorf("pool: dial %s: connection refused", leakyDSN)
		},
	}
}

func assertGenericUpstreamError(t *testing.T, output string, logs string) {
	t.Helper()
	if strings.Contains(output, "S3cretPassw0rd") || strings.Contains(output, "db.internal") || strings.Contains(output, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("client output leaks upstream error detail: %s", output)
	}
	var resp proxy.JSONRPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Split(output, "\n")[0])), &resp); err != nil {
		t.Fatalf("failed to parse response %q: %v", output, err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != errors.ErrCodeInternalError {
		t.Fatalf("expected internal error code, got %d", resp.Error.Code)
	}
	if !strings.HasPrefix(resp.Error.Message, upstreamErrorMessage) {
		t.Fatalf("expected generic message, got %q", resp.Error.Message)
	}
	cid := correlationIDRe.FindString(resp.Error.Message)
	if cid == "" {
		t.Fatalf("expected correlation id in client message %q", resp.Error.Message)
	}
	if !strings.Contains(logs, cid) {
		t.Fatalf("server log does not carry correlation id %s: %s", cid, logs)
	}
}

func routeToServer1() *mockRouter {
	return &mockRouter{
		routeFunc: func(ctx context.Context, method string) (*registry.ServerEntry, error) {
			return &registry.ServerEntry{ID: "server1"}, nil
		},
	}
}

func TestHandleSingleRequest_UpstreamErrorIsGeneric(t *testing.T) {
	logs := captureLogs(t)
	readBuf := &bytes.Buffer{}
	writer := bufio.NewWriter(readBuf)

	handleSingleRequest(context.Background(), []byte(`{"jsonrpc":"2.0","method":"test.tool","id":1}`), writer, routeToServer1(), &mockGatewayTools{}, newLeakyPool())
	writer.Flush()

	assertGenericUpstreamError(t, readBuf.String(), logs.String())
}

func TestHandleSingleRequestAsync_UpstreamErrorIsGeneric(t *testing.T) {
	logs := captureLogs(t)
	readBuf := &bytes.Buffer{}
	writer := bufio.NewWriter(readBuf)
	var mu sync.Mutex

	handleSingleRequestAsync(context.Background(), []byte(`{"jsonrpc":"2.0","method":"test.tool","id":1}`), writer, &mu, routeToServer1(), &mockGatewayTools{}, newLeakyPool())
	mu.Lock()
	writer.Flush()
	mu.Unlock()

	assertGenericUpstreamError(t, readBuf.String(), logs.String())
}

func TestHandleBatchRequest_UpstreamErrorIsGeneric(t *testing.T) {
	logs := captureLogs(t)
	readBuf := &bytes.Buffer{}
	writer := bufio.NewWriter(readBuf)

	handleBatchRequest(context.Background(), []byte(`[{"jsonrpc":"2.0","method":"test.tool","id":1}]`), writer, routeToServer1(), &mockGatewayTools{}, newLeakyPool())
	writer.Flush()

	var resps []proxy.JSONRPCResponse
	if err := json.Unmarshal(readBuf.Bytes(), &resps); err != nil || len(resps) != 1 {
		t.Fatalf("expected one batch response, got %q (%v)", readBuf.String(), err)
	}
	single, _ := json.Marshal(resps[0])
	assertGenericUpstreamError(t, string(single), logs.String())
}

func TestHandleGatewayToolSync_InvokeErrorIsGeneric(t *testing.T) {
	logs := captureLogs(t)
	gt := &mockGatewayTools{
		invokeToolFunc: func(ctx context.Context, params gateway.InvokeToolParams) (interface{}, error) {
			return nil, fmt.Errorf("invoke failed: %s", leakyDSN)
		},
	}
	req := &proxy.JSONRPCRequest{JSONRPC: "2.0", Method: "invoke_tool", ID: 1,
		Params: json.RawMessage(`{"server":"s","tool":"t","arguments":{}}`)}

	resp := handleGatewayToolSync(context.Background(), req, gt)
	out, _ := json.Marshal(resp)
	assertGenericUpstreamError(t, string(out), logs.String())
}
