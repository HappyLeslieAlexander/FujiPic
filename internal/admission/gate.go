// Package admission provides bounded, cancellable FIFO admission to expensive work.
package admission

import (
	"context"
	"errors"
	"sync"
)

var ErrFull = errors.New("admission queue full")

// Gate runs one operation at a time, retaining at most capacity waiters.
// A zero value has no waiting capacity. Call Release exactly once after success.
type Gate struct {
	mu       sync.Mutex
	active   bool
	capacity int
	waiters  []chan struct{}
}

func New(capacity int) *Gate { return &Gate{capacity: capacity} }

func (g *Gate) Acquire(ctx context.Context) error {
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return err
	}
	if !g.active {
		g.active = true
		g.mu.Unlock()
		return nil
	}
	if len(g.waiters) >= g.capacity {
		g.mu.Unlock()
		return ErrFull
	}
	ready := make(chan struct{})
	g.waiters = append(g.waiters, ready)
	g.mu.Unlock()
	select {
	case <-ready:
		if err := ctx.Err(); err != nil {
			g.Release()
			return err
		}
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		for i, ch := range g.waiters {
			if ch == ready {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				g.mu.Unlock()
				return ctx.Err()
			}
		}
		// Release transferred ownership while cancellation was being selected.
		g.releaseLocked()
		g.mu.Unlock()
		return ctx.Err()
	}
}

func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked()
}

func (g *Gate) releaseLocked() {
	if len(g.waiters) == 0 {
		g.active = false
		return
	}
	ready := g.waiters[0]
	g.waiters[0] = nil
	g.waiters = g.waiters[1:]
	close(ready)
}
