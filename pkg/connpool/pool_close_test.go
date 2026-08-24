package connpool

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

// A waiter parked on the pool must get a clean error when the pool closes,
// not a nil connection that panics in IsHealthy.
func TestServerPool_CloseWakesParkedWaiter(t *testing.T) {
	sp := NewServerPool(1, PoolConfig{MaxWaitTime: 5 * time.Second}, slog.Default())
	createFunc := func() (*client.Client, error) { return nil, nil }

	if _, err := sp.GetClient(context.Background(), createFunc); err != nil {
		t.Fatalf("prime pool: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("waiter panicked: %v", r)
				errCh <- nil
			}
		}()
		_, err := sp.GetClient(context.Background(), createFunc)
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the waiter park
	if err := sp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected pool-closed error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake on Close")
	}
}

// Returning a connection after Close must not panic (send on closed channel).
func TestServerPool_ReturnAfterCloseNoPanic(t *testing.T) {
	sp := NewServerPool(1, PoolConfig{MaxWaitTime: time.Second}, slog.Default())
	createFunc := func() (*client.Client, error) { return nil, nil }

	conn, err := sp.GetClient(context.Background(), createFunc)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	sp.ReturnClient(conn) // must not panic

	if _, err := sp.GetClient(context.Background(), createFunc); err == nil {
		t.Error("GetClient after Close should fail")
	}
}
