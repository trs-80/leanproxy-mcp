package connpool

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

// TestWaiterWakesOnReturnClient is the regression test for the dead waiting
// queue: with the pool exhausted, a waiter must receive a connection promptly
// after ReturnClient, not burn the full MaxWaitTime.
func TestWaiterWakesOnReturnClient(t *testing.T) {
	sp := NewServerPool(1, PoolConfig{MaxWaitTime: 5 * time.Second}, slog.Default())
	createFunc := func() (*client.Client, error) { return nil, nil }

	ctx := context.Background()
	conn, err := sp.GetClient(ctx, createFunc)
	if err != nil {
		t.Fatalf("first GetClient: %v", err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		sp.ReturnClient(conn)
	}()

	start := time.Now()
	conn2, err := sp.GetClient(ctx, createFunc)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waiting GetClient: %v", err)
	}
	if conn2 != conn {
		t.Fatalf("expected the returned connection to be handed to the waiter")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("waiter blocked %v; should wake promptly after ReturnClient", elapsed)
	}
}

// TestWaiterTimesOutWhenNothingReturned preserves the timeout contract.
func TestWaiterTimesOutWhenNothingReturned(t *testing.T) {
	sp := NewServerPool(1, PoolConfig{MaxWaitTime: 200 * time.Millisecond}, slog.Default())
	createFunc := func() (*client.Client, error) { return nil, nil }

	ctx := context.Background()
	if _, err := sp.GetClient(ctx, createFunc); err != nil {
		t.Fatalf("first GetClient: %v", err)
	}

	start := time.Now()
	_, err := sp.GetClient(ctx, createFunc)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("timed out too early: %v", elapsed)
	}
}
