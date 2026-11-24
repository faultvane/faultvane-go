package concurrencylimit

import (
	"context"
	"math"
	"sync/atomic"

	"github.com/faultvane/faultvane-go"
)

type Limiter struct {
	cfg       config
	inFlight  atomic.Int64
	limit     atomic.Uint64
	permitted atomic.Int64
	rejected  atomic.Int64
	decreases atomic.Int64
}

var _ faultvane.Policy = (*Limiter)(nil)

func New(opts ...Option) (*Limiter, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	l := &Limiter{cfg: cfg}
	l.limit.Store(math.Float64bits(float64(cfg.initialLimit)))
	return l, nil
}

func (l *Limiter) Limit() int { return int(math.Float64frombits(l.limit.Load())) }

func (l *Limiter) InFlight() int { return int(l.inFlight.Load()) }

func (l *Limiter) Do(ctx context.Context, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	busy, err := l.acquire()
	if err != nil {
		return err
	}
	defer l.inFlight.Add(-1)
	start := l.cfg.callStart()
	err = call(ctx)
	switch {
	case l.cfg.overloadIf(err) || l.cfg.isSlowSince(start):
		l.decreases.Add(1)
		l.adapt(l.cfg.decreased)
	case busy:
		l.adapt(l.cfg.increased)
	}
	return err
}

func (l *Limiter) acquire() (busy bool, err error) {
	limit := int64(l.Limit())
	for {
		inFlight := l.inFlight.Load()
		if inFlight >= limit {
			l.rejected.Add(1)
			return false, ErrLimitExceeded
		}
		if l.inFlight.CompareAndSwap(inFlight, inFlight+1) {
			l.permitted.Add(1)
			return 2*(inFlight+1) >= limit, nil
		}
	}
}

func (l *Limiter) adapt(change func(float64) float64) {
	for {
		current := l.limit.Load()
		if l.limit.CompareAndSwap(current, math.Float64bits(change(math.Float64frombits(current)))) {
			return
		}
	}
}
