package circuitbreaker

import (
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
)

var endOfTime = time.Unix(0, math.MaxInt64)

type view struct {
	state      State
	generation uint64
	openUntil  time.Time
	window     *window
	local      bool
	frozen     bool
}

type Breaker struct {
	name     string
	cfg      config
	origin   time.Time
	current  atomic.Pointer[view]
	pending  pending
	counters counters
	probing  atomic.Bool

	lastProbeAttempt time.Time

	mu         sync.Mutex
	record     Record
	auth       Snapshot
	hasAuth    bool
	lastWindow *window
	faults     int
	localSince time.Time
	lastToken  uint64

	syncMu    sync.Mutex
	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var _ faultvane.Policy = (*Breaker)(nil)

func New(name string, opts ...Option) (*Breaker, error) {
	cfg, err := newConfig(name, opts)
	if err != nil {
		return nil, err
	}
	b := &Breaker{name: name, cfg: cfg, origin: cfg.clock.Now()}
	if cfg.store == nil {
		b.current.Store(b.localView(nil))
		return b, nil
	}
	b.start()
	return b, nil
}

func (b *Breaker) Name() string { return b.name }

func (b *Breaker) State() State { return b.current.Load().state }

func (b *Breaker) Mode() Mode {
	if b.cfg.store != nil && !b.current.Load().local {
		return Distributed
	}
	return Local
}

func (b *Breaker) newWindow() *window {
	return newWindow(b.cfg.windowSize, b.cfg.windowBuckets)
}

func (b *Breaker) localView(reused *window) *view {
	v := &view{state: b.record.State, generation: b.record.Generation, openUntil: b.record.openUntil(), window: reused, local: true}
	if v.state == Closed && v.window == nil {
		v.window = b.newWindow()
	}
	return v
}

func (b *Breaker) publish(v *view) {
	previous := b.current.Load()
	if v.window != nil {
		b.lastWindow = v.window
	}
	b.current.Store(v)
	if previous == nil || previous.state == v.state {
		return
	}
	switch v.state {
	case Open:
		b.counters.opened.Add(1)
	case HalfOpen:
		b.counters.halfOpened.Add(1)
	default:
		b.counters.closed.Add(1)
	}
	b.emit(Event{Kind: StateChanged, From: previous.state, To: v.state, Generation: v.generation, Mode: b.Mode()})
}

func (b *Breaker) localActive() bool {
	v := b.current.Load()
	return v.local && !v.frozen
}

func (b *Breaker) applyLocal(next Record) {
	changed := next.Generation != b.record.Generation
	b.record = next
	if changed {
		b.publish(b.localView(nil))
	}
}

func (b *Breaker) Allow(ctx context.Context) (Permit, error) {
	if err := ctx.Err(); err != nil {
		return Permit{}, err
	}
	now := b.cfg.clock.Now()
	v := b.current.Load()
	switch {
	case v.state == Closed && v.window == nil:
		return Permit{}, nil
	case v.state == Closed:
		return Permit{breaker: b, view: v, start: now}, nil
	case v.state == Open && now.Before(v.openUntil):
		b.counters.rejectedOpen.Add(1)
		return Permit{}, ErrCircuitOpen
	case v.local:
		return b.acquireLocalProbe(now)
	}
	return b.acquireSharedProbe(ctx, now)
}

func (b *Breaker) probeDecision(granted bool, token uint64, now time.Time) (Permit, error) {
	v := b.current.Load()
	switch {
	case granted:
		return Permit{breaker: b, view: v, start: now, token: token}, nil
	case v.state == Closed && v.window == nil:
		return Permit{}, nil
	case v.state == Closed:
		return Permit{breaker: b, view: v, start: now}, nil
	case v.state == Open:
		b.counters.rejectedOpen.Add(1)
		return Permit{}, ErrCircuitOpen
	}
	b.counters.rejectedProbe.Add(1)
	return Permit{}, ErrProbeNotPermitted
}

func (b *Breaker) acquireLocalProbe(now time.Time) (Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.localActive() {
		return b.probeDecision(false, 0, now)
	}
	b.lastToken++
	next, granted := b.record.requestProbe(b.lastToken, now, b.cfg.halfOpenMaxProbes, b.cfg.probeTimeout)
	b.applyLocal(next)
	return b.probeDecision(granted, b.lastToken, now)
}

func (b *Breaker) acquireSharedProbe(ctx context.Context, now time.Time) (Permit, error) {
	if !b.probing.CompareAndSwap(false, true) {
		b.counters.rejectedProbe.Add(1)
		return Permit{}, ErrProbeNotPermitted
	}
	defer b.probing.Store(false)
	if now.Sub(b.lastProbeAttempt) < b.cfg.probeAttemptInterval {
		b.counters.rejectedProbe.Add(1)
		return Permit{}, ErrProbeNotPermitted
	}
	b.lastProbeAttempt = now
	token := max(rand.Uint64(), 1)
	storeCtx, cancel := context.WithTimeout(ctx, b.cfg.storeTimeout)
	grant, err := b.cfg.store.AcquireProbe(storeCtx, b.name, ProbeRequest{
		Token:         token,
		Limit:         b.cfg.halfOpenMaxProbes,
		LeaseDuration: b.cfg.probeTimeout,
		StateTTL:      b.cfg.stateTTL,
	})
	cancel()
	if err != nil {
		b.storeFailed(err)
		b.counters.rejectedProbe.Add(1)
		return Permit{}, ErrProbeNotPermitted
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults = 0
	b.adopt(grant.Snapshot)
	return b.probeDecision(grant.Granted, token, now)
}

func (b *Breaker) trip(generation uint64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.localActive() {
		b.applyLocal(b.record.trip(generation, now, b.cfg.openDuration))
	}
}

func (b *Breaker) completeLocalProbe(generation, token uint64, result Result, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.localActive() {
		b.applyLocal(b.record.completeProbe(generation, token, result, now, b.cfg.halfOpenSuccessThreshold, b.cfg.openDuration))
	}
}

func (b *Breaker) completeSharedProbe(generation, token uint64, result Result) {
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.storeTimeout)
	snapshot, err := b.cfg.store.CompleteProbe(ctx, b.name, ProbeResult{
		Generation:       generation,
		Token:            token,
		Result:           result,
		SuccessThreshold: b.cfg.halfOpenSuccessThreshold,
		OpenDuration:     b.cfg.openDuration,
		StateTTL:         b.cfg.stateTTL,
	})
	cancel()
	if err != nil {
		b.storeFailed(err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults = 0
	b.adopt(snapshot)
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
