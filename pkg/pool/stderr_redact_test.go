package pool

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestReadStderr_RedactsSecretsAndLogsAtDebug(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := newServerV2("test", StdioServerConfig{Name: "test"}, logger)

	const secret = "AKIAIOSFODNN7EXAMPLE"
	longLine := strings.Repeat("x", 5000)
	stderr := strings.NewReader("using key " + secret + "\n" + longLine + "\n")

	s.readStderr(stderr, make(chan struct{}))

	ring := s.stderrLines.String()
	if strings.Contains(ring, secret) {
		t.Fatalf("stderr ring contains raw secret: %q", ring)
	}
	if !strings.Contains(ring, "[SECRET_REDACTED]") {
		t.Fatalf("stderr ring not redacted: %q", ring)
	}

	logs := logBuf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("log output contains raw secret: %s", logs)
	}
	if !strings.Contains(logs, "level=DEBUG") || strings.Contains(logs, "level=INFO") {
		t.Fatalf("expected stderr to be logged at DEBUG only, got: %s", logs)
	}

	for _, line := range strings.Split(ring, "\n") {
		if len(line) > maxStderrLineBytes+len(stderrTruncatedMarker) {
			t.Fatalf("stderr ring line not truncated: len=%d", len(line))
		}
	}
	if !strings.Contains(ring, stderrTruncatedMarker) {
		t.Fatalf("expected truncation marker in ring: %q", ring[:80])
	}
}

func TestSendRequest_DebugLogOmitsParams(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newServerV2("test", StdioServerConfig{Name: "test"}, logger)

	// No stdin wired: sendRequest logs then fails fast on "stdin not available".
	_, err := s.sendRequest(t.Context(), Request{Method: "tools/call", Params: []byte(`{"password":"hunter2-secret"}`)}, make(chan struct{}))
	if err == nil {
		t.Fatal("expected error without stdin")
	}
	if strings.Contains(logBuf.String(), "hunter2-secret") {
		t.Fatalf("debug log leaked request params: %s", logBuf.String())
	}
}

// TestReadResponses_DebugLogOmitsStdoutLine: upstream stdout lines are tool
// results and routinely carry secrets; the debug log must not echo them.
func TestReadResponses_DebugLogOmitsStdoutLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := StdioServerConfig{
		Name:    "stdout-log",
		Command: "sh",
		// Echo the request back as the result so the secret arrives via the
		// pipe (not via the spawn command line, which is logged at startup).
		Args: []string{"-c", `read l; echo "$l"; sleep 1`},
	}
	srv := newServerV2("stdout-log", cfg, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.spawn(ctx); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer srv.stop()
	respCh := make(chan Response, 1)
	srv.pendingMu.Lock()
	srv.pending[1] = respCh
	srv.pendingMu.Unlock()
	fmt.Fprintln(srv.stdin, `{"jsonrpc":"2.0","id":1,"result":{"t":"AKIAIOSFODNN7EXAMPLE"}}`)
	select {
	case <-respCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no response")
	}
	if strings.Contains(buf.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("raw stdout line leaked into debug log:\n%s", buf.String())
	}
}
