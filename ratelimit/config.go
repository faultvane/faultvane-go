package ratelimit

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/faultvane/faultvane-go"
)

const (
	maxRate       = 1e9
	maxNameLength = 64
)

type config struct {
	rate           float64
	burst          int
	burstSet       bool
	clock          faultvane.Clock
	store          Store
	storeSet       bool
	name           string
	syncInterval   time.Duration
	storeTimeout   time.Duration
	localRate      float64
	localRateSet   bool
	faultThreshold int
	observer       Observer
	observerSet    bool
	logger         *slog.Logger
	manualSync     bool
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

func WithStore(store Store) Option {
	return func(c *config) {
		c.store = store
		c.storeSet = true
	}
}

func WithName(name string) Option {
	return func(c *config) { c.name = name }
}

func WithSyncInterval(d time.Duration) Option {
	return func(c *config) { c.syncInterval = d }
}

func WithStoreTimeout(d time.Duration) Option {
	return func(c *config) { c.storeTimeout = d }
}

func WithLocalRate(perSecond float64) Option {
	return func(c *config) {
		c.localRate = perSecond
		c.localRateSet = true
	}
}

func WithFaultThreshold(n int) Option {
	return func(c *config) { c.faultThreshold = n }
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
	c := config{
		rate:           100,
		clock:          faultvane.SystemClock(),
		syncInterval:   100 * time.Millisecond,
		storeTimeout:   50 * time.Millisecond,
		faultThreshold: 3,
		logger:         slog.Default(),
	}
	for _, opt := range opts {
		opt(&c)
	}
	if !c.localRateSet {
		c.localRate = c.rate
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
	case c.storeSet:
		return c.validateStore()
	}
	return nil
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

func (c config) validateStore() error {
	switch {
	case c.store == nil:
		return invalid("Store", "must not be nil")
	case !validName(c.name):
		return invalid("Name", "must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'")
	case c.storeTimeout <= 0:
		return invalid("StoreTimeout", "must be above 0")
	case c.syncInterval <= c.storeTimeout:
		return invalid("SyncInterval", "must be longer than StoreTimeout")
	case !(c.localRate > 0 && c.localRate <= c.rate):
		return invalid("LocalRate", "must be above 0 and at most Rate")
	case c.faultThreshold < 1:
		return invalid("FaultThreshold", "must be 1 or more")
	case c.logger == nil:
		return invalid("Logger", "must not be nil")
	case c.observerSet && c.observer == nil:
		return invalid("Observer", "must not be nil")
	}
	return nil
}
