package ratelimit

import (
	"fmt"
	"math"

	"github.com/faultvane/faultvane-go"
)

const maxRate = 1e9

type config struct {
	rate     float64
	burst    int
	burstSet bool
	clock    faultvane.Clock
}

type Option func(*config)

func WithRate(perSecond float64) Option {
	return func(c *config) { c.rate = perSecond }
}

func WithBurst(n int) Option {
	return func(c *config) {
		c.burst = n
		c.burstSet = true
	}
}

func WithClock(clock faultvane.Clock) Option {
	return func(c *config) { c.clock = clock }
}

func newConfig(opts []Option) (config, error) {
	c := config{rate: 100, clock: faultvane.SystemClock()}
	for _, opt := range opts {
		opt(&c)
	}
	if !c.burstSet {
		c.burst = int(math.Ceil(c.rate))
	}
	return c, c.validate()
}

func invalid(option, reason string) error {
	return fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, option, reason)
}

func (c config) validate() error {
	switch {
	case !(c.rate > 0 && c.rate <= maxRate):
		return invalid("Rate", "must be above 0 and at most 1e9")
	case c.burst < 1:
		return invalid("Burst", "must be 1 or more")
	case c.clock == nil:
		return invalid("Clock", "must not be nil")
	}
	return nil
}
