package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Limiter struct {
	clock  faultvane.Clock
	shared *shared

	mu        sync.Mutex
	rate      float64
	burst     float64
	since     time.Time
	tokens    float64
	permitted int64
	rejected  int64
}

type Stats struct {
	Mode        Mode
	Permitted   int64
	Rejected    int64
	StoreErrors int64
	Allowance   int64
}

var _ faultvane.Policy = (*Limiter)(nil)

func New(opts ...Option) (*Limiter, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	burst := float64(cfg.burst)
	l := &Limiter{rate: cfg.rate, burst: burst, clock: cfg.clock, since: cfg.clock.Now(), tokens: burst}
	if cfg.storeSet {
		l.start(cfg)
	}
	return l, nil
}

func (l *Limiter) available(now time.Time) float64 {
	available := l.tokens + float64(max(now.Sub(l.since), 0))*l.rate/float64(time.Second)
	if available >= l.burst {
		l.since, l.tokens = now, l.burst
		return l.burst
	}
	return available
}

func (l *Limiter) Allow() bool {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.available(now) < 1 {
		l.rejected++
		return false
	}
	l.tokens--
	l.permitted++
	return true
}

func (l *Limiter) Do(ctx context.Context, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.Allow() {
		return ErrRateLimited
	}
	return call(ctx)
}

func (l *Limiter) Stats() Stats {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	stats := Stats{Mode: Local, Permitted: l.permitted, Rejected: l.rejected, Allowance: whole(l.available(now))}
	if s := l.shared; s != nil {
		stats.Mode, stats.StoreErrors = s.mode, s.storeErrors
	}
	return stats
}
