package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Retry struct {
	cfg         config
	budget      *budget
	wait        func(context.Context, time.Duration) bool
	random      func(int64) int64
	calls       atomic.Int64
	retries     atomic.Int64
	failed      atomic.Int64
	gateStops   atomic.Int64
	budgetStops atomic.Int64
}

var _ faultvane.Policy = (*Retry)(nil)

func New(opts ...Option) (*Retry, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	r := &Retry{cfg: cfg, wait: waitFor, random: rand.Int64N}
	if cfg.budgetSet {
		r.budget = newBudget(cfg.budgetRatio)
	}
	return r, nil
}

func (r *Retry) Do(ctx context.Context, call func(context.Context) error) error {
	r.calls.Add(1)
	r.budget.deposit()
	for attempt := 1; ; attempt++ {
		err := call(ctx)
		if err == nil {
			return nil
		}
		if !r.tryAgain(ctx, err, attempt) {
			r.failed.Add(1)
			return err
		}
		r.retries.Add(1)
	}
}

func (r *Retry) tryAgain(ctx context.Context, err error, attempt int) bool {
	switch {
	case attempt == r.cfg.maxAttempts || r.final(ctx, err):
		return false
	case !r.cfg.gate():
		r.gateStops.Add(1)
		return false
	case !r.budget.withdraw():
		r.budgetStops.Add(1)
		return false
	}
	return r.wait(ctx, r.backoff(attempt))
}

func (r *Retry) final(ctx context.Context, err error) bool {
	return errors.Is(err, faultvane.ErrRejected) || ctx.Err() != nil || !r.cfg.retryIf(err)
}

func (r *Retry) backoff(attempt int) time.Duration {
	return time.Duration(r.random(int64(r.cfg.backoffCeiling(attempt))))
}

func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
