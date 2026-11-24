package circuitbreaker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
)

type snapshot struct {
	state      State
	generation uint64
	openUntil  time.Time
	window     *window
}

type Breaker struct {
	name    string
	cfg     config
	origin  time.Time
	current atomic.Pointer[snapshot]

	mu        sync.Mutex
	machine   machine
	lastToken uint64
}

var _ faultvane.Policy = (*Breaker)(nil)

func New(name string, opts ...Option) (*Breaker, error) {
	cfg, err := newConfig(name, opts)
	if err != nil {
		return nil, err
	}
	b := &Breaker{name: name, cfg: cfg, origin: cfg.clock.Now()}
	b.publish()
	return b, nil
}

func (b *Breaker) Name() string { return b.name }

func (b *Breaker) State() State { return b.current.Load().state }

func (b *Breaker) publish() {
	view := &snapshot{state: b.machine.state, generation: b.machine.generation, openUntil: b.machine.openUntil()}
	if view.state == Closed {
		view.window = newWindow(b.cfg.windowSize, b.cfg.windowBuckets)
	}
	b.current.Store(view)
}

func (b *Breaker) apply(next machine) {
	changed := next.generation != b.machine.generation
	b.machine = next
	if changed {
		b.publish()
	}
}

func (b *Breaker) Allow(ctx context.Context) (Permit, error) {
	if err := ctx.Err(); err != nil {
		return Permit{}, err
	}
	now := b.cfg.clock.Now()
	view := b.current.Load()
	switch {
	case view.state == Closed:
		return Permit{breaker: b, view: view, start: now}, nil
	case view.state == Open && now.Before(view.openUntil):
		return Permit{}, ErrCircuitOpen
	}
	return b.acquireProbe(now)
}

func (b *Breaker) acquireProbe(now time.Time) (Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine.state == Closed {
		return Permit{breaker: b, view: b.current.Load(), start: now}, nil
	}
	b.lastToken++
	next, granted := b.machine.requestProbe(b.lastToken, now, b.cfg.halfOpenMaxProbes, b.cfg.probeTimeout)
	b.apply(next)
	switch {
	case granted:
		return Permit{breaker: b, view: b.current.Load(), start: now, token: b.lastToken}, nil
	case next.state == Open:
		return Permit{}, ErrCircuitOpen
	}
	return Permit{}, ErrProbeNotPermitted
}

func (b *Breaker) trip(generation uint64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apply(b.machine.trip(generation, now, b.cfg.openDuration))
}

func (b *Breaker) completeProbe(generation, token uint64, result Result, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apply(b.machine.completeProbe(generation, token, result, now, b.cfg.halfOpenSuccessThreshold, b.cfg.openDuration))
}

func (b *Breaker) Do(ctx context.Context, call func(context.Context) error) error {
	permit, err := b.Allow(ctx)
	if err != nil {
		return err
	}
	returned := false
	defer func() {
		if !returned {
			permit.Failure()
		}
	}()
	err = call(ctx)
	returned = true
	permit.complete(b.cfg.classifier(err))
	return err
}

func Execute[T any](ctx context.Context, b *Breaker, call func(context.Context) (T, error)) (T, error) {
	var value T
	err := b.Do(ctx, func(ctx context.Context) error {
		var err error
		value, err = call(ctx)
		return err
	})
	return value, err
}
