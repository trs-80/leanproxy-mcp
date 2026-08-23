package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// delayedEchoConfig spawns the helper process in delayed-response mode: every
// request is answered concurrently after delayMs, so responses may arrive in
// any order relative to the requests.
func delayedEchoConfig(name string, delayMs int, maxConcurrent int) StdioServerConfig {
	cfg := echoServerConfig(name, "GO_HELPER_DELAY_MS="+fmt.Sprint(delayMs))
	cfg.MaxConcurrent = maxConcurrent
	return cfg
}

// TestSendRequestConcurrentOverlap proves requests to one stdio server are
// processed in parallel: N requests each taking ~delay must complete in far
// less than N*delay total.
func TestSendRequestConcurrentOverlap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process-based test in short mode")
	}
	ctx := context.Background()
	const delayMs = 400
	const n = 4
	server := newServerV2("test-overlap", delayedEchoConfig("test-overlap", delayMs, n), slog.Default())
	require.NoError(t, server.spawn(ctx))
	defer server.stop()

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = server.sendRequest(ctx, Request{Method: "ping", ID: i, Timeout: 5 * time.Second}, make(chan struct{}))
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		require.NoError(t, err, "request %d", i)
	}
	// Pigeonhole bound: serialized handling of n requests taking delayMs each
	// needs >= n*delay (1.6s), so finishing under (n-1)*delay (1.2s) is only
	// possible if at least two requests were in flight simultaneously. The
	// parallel path takes ~delay (~0.4s), leaving ~0.8s of headroom for a
	// loaded CI runner while still strictly proving overlap.
	require.Less(t, elapsed, time.Duration(n-1)*delayMs*time.Millisecond,
		"requests did not overlap: %v elapsed for %d requests of %dms", elapsed, n, delayMs)
}

// TestSendRequestOutOfOrderResponses proves each response is routed to the
// request that owns its wire ID even when responses come back out of order.
func TestSendRequestOutOfOrderResponses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process-based test in short mode")
	}
	ctx := context.Background()
	server := newServerV2("test-ooo", delayedEchoConfig("test-ooo", 150, 8), slog.Default())
	require.NoError(t, server.spawn(ctx))
	defer server.stop()

	// initialize carries a distinctive result; ping returns {}. Fire an
	// initialize first (slower to arrive is irrelevant — the delay is equal,
	// so ordering is effectively random across the batch).
	var wg sync.WaitGroup
	results := make([]json.RawMessage, 6)
	methods := []string{"initialize", "ping", "initialize", "ping", "initialize", "ping"}
	for i, m := range methods {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			res, err := server.sendRequest(ctx, Request{Method: m, ID: i, Timeout: 5 * time.Second}, make(chan struct{}))
			require.NoError(t, err)
			results[i] = res
		}(i, m)
	}
	wg.Wait()

	for i, m := range methods {
		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(results[i], &decoded))
		_, hasProto := decoded["protocolVersion"]
		if m == "initialize" {
			require.True(t, hasProto, "request %d (initialize) got wrong response: %s", i, results[i])
		} else {
			require.False(t, hasProto, "request %d (ping) got an initialize response: %s", i, results[i])
		}
	}
}
