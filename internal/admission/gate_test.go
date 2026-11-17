package admission

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func waitQueued(t *testing.T, g *Gate, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		got := len(g.waiters)
		g.mu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("waiter did not queue")
}

func TestGateFIFOAndCapacity(t *testing.T) {
	g := New(2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 2)
	for i := 1; i <= 2; i++ {
		go func() {
			if err := g.Acquire(ctx); err != nil {
				order <- -1
				return
			}
			order <- i
		}()
		waitQueued(t, g, i)
	}
	if err := g.Acquire(ctx); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	g.Release()
	if got := <-order; got != 1 {
		t.Fatal("FIFO", got)
	}
	g.Release()
	if got := <-order; got != 2 {
		t.Fatal("FIFO", got)
	}
	g.Release()
	if err := g.Acquire(ctx); err != nil {
		t.Fatal("slot leaked", err)
	}
	g.Release()
}

func TestGateCancellationRemovesWaiter(t *testing.T) {
	g := New(1)
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Acquire(ctx) }()
	waitQueued(t, g, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitQueued(t, g, 0)
	g.Release()
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.Release()
}

func TestGateCancelReleaseRaceDoesNotLeak(t *testing.T) {
	for range 100 {
		g := New(1)
		if err := g.Acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			err := g.Acquire(ctx)
			if err == nil {
				g.Release()
			}
			done <- err
		}()
		waitQueued(t, g, 1)
		go cancel()
		g.Release()
		<-done
		next, stop := context.WithTimeout(context.Background(), time.Second)
		if err := g.Acquire(next); err != nil {
			stop()
			t.Fatal("slot leaked", err)
		}
		g.Release()
		stop()
	}
}
