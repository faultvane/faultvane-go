package circuitbreaker

import "time"

type Permit struct {
	breaker *Breaker
	view    *snapshot
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
	if p.token != 0 {
		b.completeProbe(p.view.generation, p.token, result, now)
		return
	}
	if result == Ignored {
		return
	}
	elapsed := now.Sub(b.origin)
	p.view.window.record(elapsed, result == Failure, b.cfg.isSlow(now.Sub(p.start)))
	if b.cfg.rules.exceeded(p.view.window.totals(elapsed)) {
		b.trip(p.view.generation, now)
	}
}
