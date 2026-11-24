package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Limiter struct {
	rate  float64
	burst float64
	clock faultvane.Clock

	mu     sync.Mutex
	since  time.Time
	tokens float64
}

var _ faultvane.Policy = (*Limiter)(nil)

func New(opts ...Option) (*Limiter, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	burst := float64(cfg.burst)
	return &Limiter{rate: cfg.rate, burst: burst, clock: cfg.clock, since: cfg.clock.Now(), tokens: burst}, nil
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
		return false
	}
	l.tokens--
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
