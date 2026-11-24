package bulkhead

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/faultvane/faultvane-go"
)

type waiter struct {
	ready   chan struct{}
	granted bool
}

type Bulkhead struct {
	cfg config

	mu        sync.Mutex
	inFlight  int
	waiters   list.List
	permitted int64
	rejected  int64
	waited    int64
}

var _ faultvane.Policy = (*Bulkhead)(nil)

func New(opts ...Option) (*Bulkhead, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	return &Bulkhead{cfg: cfg}, nil
}

func (b *Bulkhead) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

func (b *Bulkhead) Waiting() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waiters.Len()
}

func (b *Bulkhead) Do(ctx context.Context, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()
	return call(ctx)
}

func (b *Bulkhead) acquire(ctx context.Context) error {
	b.mu.Lock()
	if b.inFlight < b.cfg.maxConcurrent {
		b.inFlight++
		b.permitted++
		b.mu.Unlock()
		return nil
	}
	if b.waiters.Len() >= b.cfg.maxWaiting {
		b.rejected++
		b.mu.Unlock()
		return ErrFull
	}
	b.waited++
	w := &waiter{ready: make(chan struct{})}
	place := b.waiters.PushBack(w)
	b.mu.Unlock()
	return b.wait(ctx, w, place)
}

func (b *Bulkhead) wait(ctx context.Context, w *waiter, place *list.Element) error {
	var waitEnded <-chan time.Time
	if b.cfg.maxWait > 0 {
		timer := time.NewTimer(b.cfg.maxWait)
		defer timer.Stop()
		waitEnded = timer.C
	}
	select {
	case <-w.ready:
	case <-ctx.Done():
	case <-waitEnded:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if w.granted {
		b.permitted++
		return nil
	}
	b.waiters.Remove(place)
	if err := ctx.Err(); err != nil {
		return err
	}
	b.rejected++
	return ErrFull
}

func (b *Bulkhead) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.passSlot()
}

func (b *Bulkhead) passSlot() {
	first := b.waiters.Front()
	if first == nil {
		b.inFlight--
		return
	}
	w := b.waiters.Remove(first).(*waiter)
	w.granted = true
	close(w.ready)
}
