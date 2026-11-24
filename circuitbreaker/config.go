package circuitbreaker

import (
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
)

const maxNameLength = 64

type config struct {
	rules                    rules
	failureThresholdSet      bool
	slowCallRateSet          bool
	slowCallDuration         time.Duration
	slowCallDurationSet      bool
	windowSize               time.Duration
	windowBuckets            int
	openDuration             time.Duration
	halfOpenMaxProbes        int
	halfOpenSuccessThreshold int
	halfOpenSuccessSet       bool
	probeTimeout             time.Duration
	classifier               Classifier
	clock                    faultvane.Clock
}

type Option func(*config)

func defaultConfig() config {
	return config{
		rules:             rules{minimumCalls: 20, failureRate: 0.5},
		windowSize:        10 * time.Second,
		windowBuckets:     10,
		openDuration:      30 * time.Second,
		halfOpenMaxProbes: 1,
		probeTimeout:      30 * time.Second,
		classifier:        DefaultClassifier,
		clock:             faultvane.SystemClock(),
	}
}

func WithFailureRateThreshold(rate float64) Option {
	return func(c *config) { c.rules.failureRate = rate }
}

func WithMinimumCalls(calls int) Option {
	return func(c *config) { c.rules.minimumCalls = int64(calls) }
}

func WithFailureThreshold(failures int) Option {
	return func(c *config) {
		c.rules.failureThreshold = int64(failures)
		c.failureThresholdSet = true
	}
}

func WithSlowCallDuration(d time.Duration) Option {
	return func(c *config) {
		c.slowCallDuration = d
		c.slowCallDurationSet = true
	}
}

func WithSlowCallRateThreshold(rate float64) Option {
	return func(c *config) {
		c.rules.slowCallRate = rate
		c.slowCallRateSet = true
	}
}

func WithWindowSize(d time.Duration) Option {
	return func(c *config) { c.windowSize = d }
}

func WithWindowBuckets(buckets int) Option {
	return func(c *config) { c.windowBuckets = buckets }
}

func WithOpenDuration(d time.Duration) Option {
	return func(c *config) { c.openDuration = d }
}

func WithHalfOpenMaxProbes(probes int) Option {
	return func(c *config) { c.halfOpenMaxProbes = probes }
}

func WithHalfOpenSuccessThreshold(successes int) Option {
	return func(c *config) {
		c.halfOpenSuccessThreshold = successes
		c.halfOpenSuccessSet = true
	}
}

func WithProbeTimeout(d time.Duration) Option {
	return func(c *config) { c.probeTimeout = d }
}

func WithClassifier(classifier Classifier) Option {
	return func(c *config) { c.classifier = classifier }
}

func WithClock(clock faultvane.Clock) Option {
	return func(c *config) { c.clock = clock }
}

func newConfig(name string, opts []Option) (config, error) {
	c := defaultConfig()
	for _, opt := range opts {
		opt(&c)
	}
	if !c.halfOpenSuccessSet {
		c.halfOpenSuccessThreshold = c.halfOpenMaxProbes
	}
	return c, c.validate(name)
}

func invalid(option, reason string) error {
	return fmt.Errorf("%w: %s %s", faultvane.ErrInvalidConfig, option, reason)
}

func validRate(rate float64) bool {
	return rate > 0 && rate <= 1
}

func validName(name string) bool {
	if name == "" || len(name) > maxNameLength {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (c config) validate(name string) error {
	switch {
	case !validName(name):
		return invalid("name", "must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'")
	case !validRate(c.rules.failureRate):
		return invalid("FailureRateThreshold", "must be above 0 and at most 1")
	case c.rules.minimumCalls < 1:
		return invalid("MinimumCalls", "must be 1 or more")
	case c.failureThresholdSet && c.rules.failureThreshold < 1:
		return invalid("FailureThreshold", "must be 1 or more")
	case c.slowCallRateSet != c.slowCallDurationSet:
		return invalid("SlowCallDuration and SlowCallRateThreshold", "must be set together")
	case c.slowCallDurationSet && c.slowCallDuration <= 0:
		return invalid("SlowCallDuration", "must be above 0")
	case c.slowCallRateSet && !validRate(c.rules.slowCallRate):
		return invalid("SlowCallRateThreshold", "must be above 0 and at most 1")
	case c.windowBuckets < 1:
		return invalid("WindowBuckets", "must be 1 or more")
	case c.windowSize < time.Duration(c.windowBuckets):
		return invalid("WindowSize", "must be above 0 and give each bucket a duration above 0")
	case c.openDuration <= 0:
		return invalid("OpenDuration", "must be above 0")
	case c.halfOpenMaxProbes < 1:
		return invalid("HalfOpenMaxProbes", "must be 1 or more")
	case c.halfOpenSuccessThreshold < 1:
		return invalid("HalfOpenSuccessThreshold", "must be 1 or more")
	case c.probeTimeout <= 0:
		return invalid("ProbeTimeout", "must be above 0")
	case c.classifier == nil:
		return invalid("Classifier", "must not be nil")
	case c.clock == nil:
		return invalid("Clock", "must not be nil")
	}
	return nil
}

func (c config) isSlow(d time.Duration) bool {
	return c.slowCallDurationSet && d >= c.slowCallDuration
}
