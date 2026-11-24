package circuitbreaker

import "time"

type Permit struct {
	breaker *Breaker
	view    *view
	start   time.Time
	token   uint64
}

func (p Permit) Success() { p.complete(Success) }

func (p Permit) Failure() { p.complete(Failure) }

func (p Permit) Ignore() { p.complete(Ignored) }

func (p Permit) complete(result Result) {
	b := p.breaker
	if b == nil {
		return
	}
	now := b.cfg.clock.Now()
	switch result {
	case Success:
		b.counters.successes.Add(1)
	case Failure:
		b.counters.failures.Add(1)
	default:
		b.counters.ignored.Add(1)
	}
	if p.token != 0 {
		p.completeProbe(result, now)
		return
	}
	if result == Ignored {
		return
	}
	failed, slow := result == Failure, b.cfg.isSlow(now.Sub(p.start))
	if slow {
		b.counters.slowCalls.Add(1)
	}
	elapsed := now.Sub(b.origin)
	p.view.window.record(elapsed, failed, slow)
	if b.cfg.store != nil && b.current.Load() == p.view {
		b.pending.add(failed, slow)
	}
	switch {
	case p.view.local:
		if b.cfg.localThresholds.Exceeded(p.view.window.totals(elapsed)) {
			b.trip(p.view.generation, now)
		}
	case failed:
		b.wakeSynchronizer()
	}
}

func (p Permit) completeProbe(result Result, now time.Time) {
	b := p.breaker
	switch result {
	case Success:
		b.counters.probeSuccesses.Add(1)
	case Failure:
		b.counters.probeFailures.Add(1)
	}
	if p.view.local {
		b.completeLocalProbe(p.view.generation, p.token, result, now)
		return
	}
	b.completeSharedProbe(p.view.generation, p.token, result)
}
