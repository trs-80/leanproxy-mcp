package pool

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// currentLoad must reflect requests dispatched through the real requestCh
// path: checkIdleTimeout's currentLoad==0 guard is the only thing standing
// between the idle reaper and a SIGTERM while a slow request is mid-flight
// (state can be back to idle when a fast request finished after claiming busy).
func TestDispatchPathMaintainsCurrentLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process-based test in short mode")
	}
	ctx := context.Background()
	server := newServerV2("test-load", delayedEchoConfig("test-load", 600, 2), slog.Default())
	require.NoError(t, server.spawn(ctx))
	defer server.stop()

	resultCh := make(chan *Response, 1)
	server.requestCh <- Request{Method: "ping", ID: 41, Timeout: 5 * time.Second, ResultCh: resultCh}

	time.Sleep(200 * time.Millisecond) // request is mid-flight (600ms delay)
	server.mu.Lock()
	load := server.currentLoad
	server.mu.Unlock()
	require.Equal(t, 1, load, "in-flight request not counted; idle reaper could SIGTERM mid-request")

	select {
	case resp := <-resultCh:
		require.Nil(t, resp.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed")
	}

	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.currentLoad == 0
	}, 2*time.Second, 20*time.Millisecond, "load not released after completion")
}

// A request already dequeued (or still buffered) when the server stops must
// be answered with a fast error, not silently dropped leaving the caller to
// burn its full timeout.
func TestStopFailsPendingRequestsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process-based test in short mode")
	}
	ctx := context.Background()
	// maxConcurrent=1: the first slow request holds the only dispatch slot.
	server := newServerV2("test-stopfail", delayedEchoConfig("test-stopfail", 1500, 1), slog.Default())
	require.NoError(t, server.spawn(ctx))

	slowCh := make(chan *Response, 1)
	server.requestCh <- Request{Method: "ping", ID: 1, Timeout: 10 * time.Second, ResultCh: slowCh}
	time.Sleep(150 * time.Millisecond) // slow request occupies the sem

	blockedCh := make(chan *Response, 1)
	server.requestCh <- Request{Method: "ping", ID: 2, Timeout: 10 * time.Second, ResultCh: blockedCh}
	time.Sleep(150 * time.Millisecond) // loop dequeues it and parks on the sem

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = server.stop() }()

	start := time.Now()
	select {
	case resp := <-blockedCh:
		require.NotNil(t, resp.Error, "expected an error response for the undispatched request")
		require.True(t, strings.Contains(resp.Error.Message, "stopping"), "got: %s", resp.Error.Message)
		require.Less(t, time.Since(start), 3*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("dequeued request was dropped: caller would burn its full timeout")
	}
	wg.Wait()
	_ = ctx
}
