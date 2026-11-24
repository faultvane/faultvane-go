package retry

import (
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
)

type config struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	retryIf        func(error) bool
	gate           func() bool
	budgetRatio    float64
	budgetSet      bool
}

type Option func(*config)

func defaultConfig() config {
	return config{
		maxAttempts:    3,
		initialBackoff: 100 * time.Millisecond,
		maxBackoff:     10 * time.Second,
		retryIf:        func(error) bool { return true },
		gate:           func() bool { return true },
	}
}

func WithMaxAttempts(attempts int) Option {
	return func(c *config) { c.maxAttempts = attempts }
}

func WithBackoff(initial, maximum time.Duration) Option {
	return func(c *config) {
		c.initialBackoff = initial
		c.maxBackoff = maximum
	}
}

func WithRetryIf(retryIf func(error) bool) Option {
	return func(c *config) { c.retryIf = retryIf }
}

func WithGate(gate func() bool) Option {
	return func(c *config) { c.gate = gate }
}

func WithBudget(ratio float64) Option {
	return func(c *config) {
		c.budgetRatio = ratio
		c.budgetSet = true
	}
}

func newConfig(opts []Option) (config, error) {
	c := defaultConfig()
	for _, opt := range opts {
		opt(&c)
	}
	return c, c.validate()
}

func invalid(option, reason string) error {
	return fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, option, reason)
}

func validRatio(ratio float64) bool {
	return ratio > 0 && ratio <= 1
}

func (c config) validate() error {
	switch {
	case c.maxAttempts < 1:
		return invalid("MaxAttempts", "must be 1 or more")
	case c.initialBackoff <= 0:
		return invalid("Backoff", "must have an initial duration above 0")
	case c.maxBackoff < c.initialBackoff:
		return invalid("Backoff", "must have a maximum duration that is not below the initial duration")
	case c.retryIf == nil:
		return invalid("RetryIf", "must not be nil")
	case c.gate == nil:
		return invalid("Gate", "must not be nil")
	case c.budgetSet && !validRatio(c.budgetRatio):
		return invalid("Budget", "must be above 0 and at most 1")
	}
	return nil
}

func (c config) backoffCeiling(attempt int) time.Duration {
	if doublings := attempt - 1; c.initialBackoff <= c.maxBackoff>>doublings {
		return c.initialBackoff << doublings
	}
	return c.maxBackoff
}
