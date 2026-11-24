package concurrencylimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/faultvane/faultvane-go"
)

type config struct {
	initialLimit        int
	minLimit            int
	maxLimit            int
	latencyThreshold    time.Duration
	latencyThresholdSet bool
	backoffRatio        float64
	overloadIf          func(error) bool
	clock               faultvane.Clock
	observer            Observer
	observerSet         bool
	logger              *slog.Logger
}

type Option func(*config)

func deadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func defaultConfig() config {
	return config{
		initialLimit: 20,
		minLimit:     1,
		maxLimit:     200,
		backoffRatio: 0.9,
		overloadIf:   deadlineExceeded,
		clock:        faultvane.SystemClock(),
		logger:       slog.Default(),
	}
}

func WithInitialLimit(calls int) Option {
	return func(c *config) { c.initialLimit = calls }
}

func WithMinLimit(calls int) Option {
	return func(c *config) { c.minLimit = calls }
}

func WithMaxLimit(calls int) Option {
	return func(c *config) { c.maxLimit = calls }
}

func WithLatencyThreshold(d time.Duration) Option {
	return func(c *config) {
		c.latencyThreshold = d
		c.latencyThresholdSet = true
	}
}

func WithBackoffRatio(ratio float64) Option {
	return func(c *config) { c.backoffRatio = ratio }
}

func WithOverloadIf(overload func(error) bool) Option {
	return func(c *config) { c.overloadIf = overload }
}

func WithClock(clock faultvane.Clock) Option {
	return func(c *config) { c.clock = clock }
}

func WithObserver(observer Observer) Option {
	return func(c *config) {
		c.observer = observer
		c.observerSet = true
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(c *config) { c.logger = logger }
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

func (c config) validate() error {
	switch {
	case c.minLimit < 1:
		return invalid("MinLimit", "must be 1 or more")
	case c.maxLimit < c.minLimit:
		return invalid("MaxLimit", "must be MinLimit or more")
	case c.initialLimit < c.minLimit || c.initialLimit > c.maxLimit:
		return invalid("InitialLimit", "must be from MinLimit to MaxLimit")
	case c.latencyThresholdSet && c.latencyThreshold <= 0:
		return invalid("LatencyThreshold", "must be above 0")
	case !(c.backoffRatio > 0 && c.backoffRatio < 1):
		return invalid("BackoffRatio", "must be above 0 and below 1")
	case c.overloadIf == nil:
		return invalid("OverloadIf", "must not be nil")
	case c.clock == nil:
		return invalid("Clock", "must not be nil")
	case c.logger == nil:
		return invalid("Logger", "must not be nil")
	case c.observerSet && c.observer == nil:
		return invalid("Observer", "must not be nil")
	}
	return nil
}

func (c config) callStart() time.Time {
	if !c.latencyThresholdSet {
		return time.Time{}
	}
	return c.clock.Now()
}

func (c config) isSlowSince(start time.Time) bool {
	return c.latencyThresholdSet && c.clock.Now().Sub(start) >= c.latencyThreshold
}

func (c config) decreased(limit float64) float64 {
	return max(float64(c.minLimit), limit*c.backoffRatio)
}

func (c config) increased(limit float64) float64 {
	return min(float64(c.maxLimit), limit+1/limit)
}
