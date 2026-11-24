package bulkhead

import (
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
)

type config struct {
	maxConcurrent int
	maxWaiting    int
	maxWait       time.Duration
}

type Option func(*config)

func WithMaxConcurrent(calls int) Option {
	return func(c *config) { c.maxConcurrent = calls }
}

func WithMaxWaiting(callers int) Option {
	return func(c *config) { c.maxWaiting = callers }
}

func WithMaxWait(d time.Duration) Option {
	return func(c *config) { c.maxWait = d }
}

func newConfig(opts []Option) (config, error) {
	c := config{maxConcurrent: 10}
	for _, opt := range opts {
		opt(&c)
	}
	return c, c.validate()
}

func invalid(option, reason string) error {
	return fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, option, reason)
}

func (c config) validate() error {
	switch {
	case c.maxConcurrent < 1:
		return invalid("MaxConcurrent", "must be 1 or more")
	case c.maxWaiting < 0:
		return invalid("MaxWaiting", "must be 0 or more")
	case c.maxWait < 0:
		return invalid("MaxWait", "must be 0 or more")
	}
	return nil
}
