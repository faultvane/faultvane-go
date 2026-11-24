package loadshed

import (
	"fmt"

	"github.com/faultvane/faultvane-go"
)

type config struct {
	maxInFlight     int
	thresholds      [priorities]float64
	unknownPriority bool
}

type Option func(*config)

func WithMaxInFlight(n int) Option {
	return func(c *config) { c.maxInFlight = n }
}

func WithThreshold(p Priority, utilization float64) Option {
	return func(c *config) {
		if p.valid() {
			c.thresholds[p] = utilization
		} else {
			c.unknownPriority = true
		}
	}
}

func newConfig(opts []Option) (config, error) {
	c := config{maxInFlight: 100, thresholds: [priorities]float64{Critical: 1, High: 0.9, Normal: 0.75, Low: 0.5}}
	for _, opt := range opts {
		opt(&c)
	}
	return c, c.validate()
}

func invalid(option, reason string) error {
	return fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, option, reason)
}

func (c config) validate() error {
	if c.maxInFlight < 1 {
		return invalid("MaxInFlight", "must be 1 or more")
	}
	if c.unknownPriority {
		return invalid("Threshold", "must have the priority Critical, High, Normal, or Low")
	}
	for _, threshold := range c.thresholds {
		if !(threshold > 0 && threshold <= 1) {
			return invalid("Threshold", "must be above 0 and at most 1")
		}
	}
	for p := Critical; p < Low; p++ {
		if c.thresholds[p] < c.thresholds[p+1] {
			return invalid("Threshold", "must not decrease from Low to Critical")
		}
	}
	return nil
}
