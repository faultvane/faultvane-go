package timeout

import (
	"context"
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Timeout struct {
	limit time.Duration
}

var _ faultvane.Policy = (*Timeout)(nil)

func New(d time.Duration) (*Timeout, error) {
	if d <= 0 {
		return nil, fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, "duration", "must be above 0")
	}
	return &Timeout{limit: d}, nil
}

func (t *Timeout) Do(ctx context.Context, call func(context.Context) error) error {
	limited, cancel := context.WithTimeout(ctx, t.limit)
	defer cancel()
	err := call(limited)
	if err == nil || limited.Err() == nil || ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrTimeout, err)
}
