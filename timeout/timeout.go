package timeout

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Timeout struct {
	limit    time.Duration
	calls    atomic.Int64
	timeouts atomic.Int64
}

var _ faultvane.Policy = (*Timeout)(nil)

func New(d time.Duration) (*Timeout, error) {
	if d <= 0 {
		return nil, fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, "duration", "must be above 0")
	}
	return &Timeout{limit: d}, nil
}

func (t *Timeout) Do(ctx context.Context, call func(context.Context) error) error {
	t.calls.Add(1)
	limited, cancel := context.WithTimeout(ctx, t.limit)
	defer cancel()
	err := call(limited)
	if err == nil || limited.Err() == nil || ctx.Err() != nil {
		return err
	}
	t.timeouts.Add(1)
	return fmt.Errorf("%w: %w", ErrTimeout, err)
}
