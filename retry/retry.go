package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Retry struct {
	cfg    config
	budget *budget
	wait   func(context.Context, time.Duration) bool
	random func(int64) int64
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
	r.budget.deposit()
	for attempt := 1; ; attempt++ {
		err := call(ctx)
		if err == nil || attempt == r.cfg.maxAttempts || r.final(ctx, err) || !r.cfg.gate() || !r.budget.withdraw() || !r.wait(ctx, r.backoff(attempt)) {
			return err
		}
	}
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
