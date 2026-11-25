package circuitbreaker

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

type counts struct {
	calls     int64
	failures  int64
	slowCalls int64
}

func (c counts) zero() bool {
	return c.calls == 0
}

type pending struct {
	calls     atomic.Int64
	failures  atomic.Int64
	slowCalls atomic.Int64
}

func (p *pending) add(failed, slow bool) {
	p.calls.Add(1)
	if failed {
		p.failures.Add(1)
	}
	if slow {
		p.slowCalls.Add(1)
	}
}

func (p *pending) take() counts {
	return counts{calls: p.calls.Swap(0), failures: p.failures.Swap(0), slowCalls: p.slowCalls.Swap(0)}
}

func (p *pending) restore(c counts) {
	p.calls.Add(c.calls)
	p.failures.Add(c.failures)
	p.slowCalls.Add(c.slowCalls)
}

func (b *Breaker) start() {
	b.wake = make(chan struct{}, 1)
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.startTimeout)
	snapshot, err := b.cfg.store.Load(ctx, b.name)
	cancel()
	b.mu.Lock()
	if err == nil {
		b.adopt(snapshot)
	} else {
		b.counters.storeErrors.Add(1)
		b.emit(Event{Kind: StoreFault, Mode: Local, Err: err})
		b.startLocal()
	}
	b.mu.Unlock()
	if b.cfg.manualSync {
		close(b.done)
		return
	}
	go b.run()
}

func (b *Breaker) startLocal() {
	b.localSince = b.cfg.clock.Now()
	b.counters.localModeEntries.Add(1)
	switch b.cfg.startPolicy {
	case StartWithProbe:
		b.record = Record{State: HalfOpen}
		b.current.Store(b.localView(nil))
	case StartClosed:
		b.current.Store(b.localView(nil))
	default:
		b.current.Store(&view{state: Open, openUntil: endOfTime, local: true, frozen: true})
	}
	b.emit(Event{Kind: ModeChanged, To: b.State(), Mode: Local})
}

func (b *Breaker) adopt(snapshot Snapshot) {
	previous := b.current.Load()
	fresh := previous == nil || previous.local || !b.hasAuth || snapshot.Generation != b.auth.Generation || snapshot.State != previous.state
	wasLocal := previous != nil && previous.local
	newGeneration := snapshot.Generation != b.auth.Generation
	b.auth, b.hasAuth = snapshot, true
	b.compareConfig(snapshot)
	if !fresh {
		return
	}
	remaining := snapshot.TransitionAt.Add(snapshot.OpenDuration).Sub(snapshot.StoreTime)
	v := &view{state: snapshot.State, generation: snapshot.Generation, openUntil: b.cfg.clock.Now().Add(remaining)}
	if v.state == Closed {
		v.window = b.newWindow()
	}
	b.publish(v)
	if newGeneration {
		b.pending.take()
	}
	if wasLocal {
		b.counters.localModeExpired.Store(false)
		b.emit(Event{Kind: ModeChanged, From: previous.state, To: v.state, Generation: v.generation, Mode: Distributed})
	}
}

const mismatchEndResponses = 10

func (b *Breaker) compareConfig(snapshot Snapshot) {
	different := snapshot.Fingerprint != "" && snapshot.Fingerprint != b.fingerprint
	b.responsesWithoutMismatch++
	if different {
		b.responsesWithoutMismatch = 0
	}
	mismatch := different || b.counters.configMismatch.Load() && b.responsesWithoutMismatch < mismatchEndResponses
	if b.counters.configMismatch.Swap(mismatch) != mismatch {
		b.emit(Event{Kind: ConfigMismatchChanged, From: snapshot.State, To: snapshot.State, Generation: snapshot.Generation, Mode: Distributed, ConfigMismatch: mismatch})
	}
}

func (b *Breaker) storeFailed(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.storeErrors.Add(1)
	b.emit(Event{Kind: StoreFault, Mode: b.Mode(), Err: err})
	b.faults++
	if b.faults >= b.cfg.faultThreshold && !b.current.Load().local {
		b.enterLocal(err)
	}
}

func (b *Breaker) enterLocal(cause error) {
	previous := b.current.Load()
	b.localSince = b.cfg.clock.Now()
	b.counters.localModeEntries.Add(1)
	b.record = Record{State: previous.state, Generation: previous.generation, TransitionAt: previous.openUntil}
	b.current.Store(b.localView(previous.window))
	b.emit(Event{Kind: ModeChanged, From: previous.state, To: previous.state, Generation: previous.generation, Mode: Local, Err: cause})
	if !b.cfg.localFallback {
		b.expireLocal()
	}
}

func (b *Breaker) expireLocal() {
	b.counters.localModeExpired.Store(true)
	previous := b.current.Load()
	b.emit(Event{Kind: LocalModeExpired, From: previous.state, To: previous.state, Generation: previous.generation, Mode: Local})
	switch b.cfg.expiredPolicy {
	case RejectAll:
		b.publish(&view{state: Open, generation: previous.generation, openUntil: endOfTime, local: true, frozen: true})
	case AllowAll:
		b.publish(&view{state: Closed, generation: previous.generation, local: true, frozen: true})
	}
}

func (b *Breaker) report(generation uint64, c counts) Report {
	return Report{
		Generation:    generation,
		Calls:         c.calls,
		Failures:      c.failures,
		SlowCalls:     c.slowCalls,
		Thresholds:    b.cfg.thresholds,
		WindowSize:    b.cfg.windowSize,
		WindowBuckets: b.cfg.windowBuckets,
		OpenDuration:  b.cfg.openDuration,
		StateTTL:      b.cfg.stateTTL,
		Fingerprint:   b.fingerprint,
	}
}

func (b *Breaker) authoritative() (Snapshot, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auth, b.hasAuth
}

func (b *Breaker) restoreIfLost(ctx context.Context, snapshot Snapshot) (Snapshot, error) {
	auth, known := b.authoritative()
	if !known || snapshot.Generation >= auth.Generation {
		return snapshot, nil
	}
	return b.cfg.store.Restore(ctx, b.name, auth)
}

func (b *Breaker) synchronize(ctx context.Context) {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, b.cfg.storeTimeout)
	defer cancel()
	v := b.current.Load()
	if v.local {
		b.recoverStore(ctx)
		return
	}
	var snapshot Snapshot
	var err error
	if c := b.pending.take(); c.zero() {
		snapshot, err = b.cfg.store.Load(ctx, b.name)
	} else if snapshot, err = b.cfg.store.Report(ctx, b.name, b.report(v.generation, c)); err != nil {
		b.pending.restore(c)
	}
	if err == nil {
		snapshot, err = b.restoreIfLost(ctx, snapshot)
	}
	if err != nil {
		b.storeFailed(err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults = 0
	b.adopt(snapshot)
}

func (b *Breaker) recoverStore(ctx context.Context) {
	b.mu.Lock()
	expired := !b.counters.localModeExpired.Load() && b.cfg.clock.Now().Sub(b.localSince) >= b.cfg.maxLocalFallbackDuration
	if expired {
		b.expireLocal()
	}
	auth, known, lastWindow := b.auth, b.hasAuth, b.lastWindow
	b.mu.Unlock()

	snapshot, err := b.cfg.store.Load(ctx, b.name)
	if err == nil {
		snapshot, err = b.restoreIfLost(ctx, snapshot)
	}
	if err != nil {
		b.counters.storeErrors.Add(1)
		return
	}
	c := b.pending.take()
	if known && lastWindow != nil && snapshot.Generation == auth.Generation && snapshot.State == Closed {
		calls, failures, slowCalls := lastWindow.totals(b.cfg.clock.Now().Sub(b.origin))
		c = counts{calls: min(c.calls, calls), failures: min(c.failures, failures), slowCalls: min(c.slowCalls, slowCalls)}
		if !c.zero() {
			if snapshot, err = b.cfg.store.Report(ctx, b.name, b.report(auth.Generation, c)); err != nil {
				b.pending.restore(c)
				b.counters.storeErrors.Add(1)
				return
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults = 0
	b.counters.reconciliations.Add(1)
	b.emit(Event{Kind: Reconciled, From: b.State(), To: snapshot.State, Generation: snapshot.Generation, Mode: Distributed})
	b.adopt(snapshot)
}

func (b *Breaker) wakeSynchronizer() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Breaker) notified() {
	b.counters.notifications.Add(1)
	if !b.current.Load().local {
		b.wakeSynchronizer()
	}
}

func (b *Breaker) subscribe() func() {
	notifier, ok := b.cfg.store.(Notifier)
	if !ok {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.storeTimeout)
	defer cancel()
	stop, err := notifier.Subscribe(ctx, b.name, b.notified)
	if err != nil {
		b.counters.subscribeFailed.Store(true)
		b.cfg.logger.Warn("circuit breaker subscription to transition notifications failed", "breaker", b.name, "cause", err.Error())
		return func() {}
	}
	return stop
}

func (b *Breaker) wait(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-b.stop:
		return false
	case <-timer.C:
		return true
	}
}

func earlyReportWait(interval, sinceLastReport time.Duration) time.Duration {
	return interval - sinceLastReport
}

func healthCheckDelay(limit time.Duration) time.Duration {
	return limit/2 + rand.N(limit/2+1)
}

func (b *Breaker) run() {
	defer close(b.done)
	unsubscribe := b.subscribe()
	defer unsubscribe()
	delay, backoff := b.cfg.syncInterval, b.cfg.syncInterval/2
	last := time.Now()
	for {
		timer := time.NewTimer(delay)
		select {
		case <-b.stop:
			timer.Stop()
			return
		case <-timer.C:
		case <-b.wake:
			timer.Stop()
			if !b.wait(earlyReportWait(b.cfg.earlyReportInterval, time.Since(last))) {
				return
			}
		}
		b.synchronize(context.Background())
		last = time.Now()
		if b.current.Load().local {
			delay = healthCheckDelay(backoff)
			backoff = min(backoff*2, 10*b.cfg.syncInterval)
		} else {
			delay, backoff = b.cfg.syncInterval, b.cfg.syncInterval/2
		}
	}
}

func (b *Breaker) Close(ctx context.Context) error {
	if b.cfg.store == nil {
		return nil
	}
	b.closeOnce.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !b.current.Load().local {
		b.synchronize(ctx)
	}
	return nil
}
