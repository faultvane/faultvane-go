package circuitbreaker

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/faultvane/faultvane-go"
)

const maxNameLength = 64

type ExpiredPolicy uint8

const (
	KeepLocalBreaker ExpiredPolicy = iota
	RejectAll
	AllowAll
)

type StartPolicy uint8

const (
	StartWithProbe StartPolicy = iota
	StartClosed
	StartRejecting
)

type config struct {
	thresholds               Thresholds
	localThresholds          Thresholds
	failureThresholdSet      bool
	localFailureThresholdSet bool
	localMinimumCallsSet     bool
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
	probeAttemptInterval     time.Duration
	classifier               Classifier
	clock                    faultvane.Clock
	store                    Store
	storeSet                 bool
	storeTimeout             time.Duration
	syncInterval             time.Duration
	earlyReportInterval      time.Duration
	faultThreshold           int
	startTimeout             time.Duration
	stateTTL                 time.Duration
	localFallback            bool
	maxLocalFallbackDuration time.Duration
	expiredPolicy            ExpiredPolicy
	startPolicy              StartPolicy
	observer                 Observer
	observerSet              bool
	logger                   *slog.Logger
	manualSync               bool
}

type Option func(*config)

func defaultConfig() config {
	return config{
		thresholds:               Thresholds{MinimumCalls: 20, FailureRate: 0.5},
		windowSize:               10 * time.Second,
		windowBuckets:            10,
		openDuration:             30 * time.Second,
		halfOpenMaxProbes:        1,
		probeTimeout:             30 * time.Second,
		probeAttemptInterval:     100 * time.Millisecond,
		classifier:               DefaultClassifier,
		clock:                    faultvane.SystemClock(),
		storeTimeout:             100 * time.Millisecond,
		syncInterval:             500 * time.Millisecond,
		earlyReportInterval:      50 * time.Millisecond,
		faultThreshold:           3,
		startTimeout:             time.Second,
		stateTTL:                 time.Hour,
		localFallback:            true,
		maxLocalFallbackDuration: 5 * time.Minute,
		logger:                   slog.Default(),
	}
}

func WithFailureRateThreshold(rate float64) Option {
	return func(c *config) { c.thresholds.FailureRate = rate }
}

func WithMinimumCalls(calls int) Option {
	return func(c *config) { c.thresholds.MinimumCalls = int64(calls) }
}

func WithFailureThreshold(failures int) Option {
	return func(c *config) {
		c.thresholds.FailureThreshold = int64(failures)
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
		c.thresholds.SlowCallRate = rate
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

func WithProbeAttemptInterval(d time.Duration) Option {
	return func(c *config) { c.probeAttemptInterval = d }
}

func WithClassifier(classifier Classifier) Option {
	return func(c *config) { c.classifier = classifier }
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

func WithStoreTimeout(d time.Duration) Option {
	return func(c *config) { c.storeTimeout = d }
}

func WithSyncInterval(d time.Duration) Option {
	return func(c *config) { c.syncInterval = d }
}

func WithEarlyReportInterval(d time.Duration) Option {
	return func(c *config) { c.earlyReportInterval = d }
}

func WithFaultThreshold(faults int) Option {
	return func(c *config) { c.faultThreshold = faults }
}

func WithStartTimeout(d time.Duration) Option {
	return func(c *config) { c.startTimeout = d }
}

func WithStateTTL(d time.Duration) Option {
	return func(c *config) { c.stateTTL = d }
}

func WithLocalFallback(enabled bool) Option {
	return func(c *config) { c.localFallback = enabled }
}

func WithMaxLocalFallbackDuration(d time.Duration) Option {
	return func(c *config) { c.maxLocalFallbackDuration = d }
}

func WithLocalModeExpiredPolicy(policy ExpiredPolicy) Option {
	return func(c *config) { c.expiredPolicy = policy }
}

func WithStartPolicy(policy StartPolicy) Option {
	return func(c *config) { c.startPolicy = policy }
}

func WithLocalFailureThreshold(failures int) Option {
	return func(c *config) {
		c.localThresholds.FailureThreshold = int64(failures)
		c.localFailureThresholdSet = true
	}
}

func WithLocalMinimumCalls(calls int) Option {
	return func(c *config) {
		c.localThresholds.MinimumCalls = int64(calls)
		c.localMinimumCallsSet = true
	}
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

func newConfig(name string, opts []Option) (config, error) {
	c := defaultConfig()
	for _, opt := range opts {
		opt(&c)
	}
	if !c.halfOpenSuccessSet {
		c.halfOpenSuccessThreshold = c.halfOpenMaxProbes
	}
	local := c.thresholds
	if c.localFailureThresholdSet {
		local.FailureThreshold = c.localThresholds.FailureThreshold
	}
	if c.localMinimumCallsSet {
		local.MinimumCalls = c.localThresholds.MinimumCalls
	}
	c.localThresholds = local
	c.stateTTL = max(c.stateTTL, 2*(c.openDuration+c.windowSize))
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
	case !validRate(c.thresholds.FailureRate):
		return invalid("FailureRateThreshold", "must be above 0 and at most 1")
	case c.thresholds.MinimumCalls < 1:
		return invalid("MinimumCalls", "must be 1 or more")
	case c.failureThresholdSet && c.thresholds.FailureThreshold < 1:
		return invalid("FailureThreshold", "must be 1 or more")
	case c.slowCallRateSet != c.slowCallDurationSet:
		return invalid("SlowCallDuration and SlowCallRateThreshold", "must be set together")
	case c.slowCallDurationSet && c.slowCallDuration <= 0:
		return invalid("SlowCallDuration", "must be above 0")
	case c.slowCallRateSet && !validRate(c.thresholds.SlowCallRate):
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
	case c.probeAttemptInterval < 0:
		return invalid("ProbeAttemptInterval", "must be 0 or more")
	case c.classifier == nil:
		return invalid("Classifier", "must not be nil")
	case c.clock == nil:
		return invalid("Clock", "must not be nil")
	case c.logger == nil:
		return invalid("Logger", "must not be nil")
	case c.observerSet && c.observer == nil:
		return invalid("Observer", "must not be nil")
	case c.localFailureThresholdSet && c.localThresholds.FailureThreshold < 1:
		return invalid("LocalFailureThreshold", "must be 1 or more")
	case c.localThresholds.MinimumCalls < 1:
		return invalid("LocalMinimumCalls", "must be 1 or more")
	case c.storeSet:
		return c.validateStore()
	}
	return nil
}

func (c config) validateStore() error {
	switch {
	case c.store == nil:
		return invalid("Store", "must not be nil")
	case c.storeTimeout <= 0:
		return invalid("StoreTimeout", "must be above 0")
	case c.syncInterval <= c.storeTimeout:
		return invalid("SyncInterval", "must be longer than StoreTimeout")
	case c.earlyReportInterval <= 0 || c.earlyReportInterval > c.syncInterval:
		return invalid("EarlyReportInterval", "must be above 0 and not longer than SyncInterval")
	case c.windowSize/time.Duration(c.windowBuckets) < c.syncInterval:
		return invalid("WindowSize", "must give each bucket a duration that is not shorter than SyncInterval")
	case c.faultThreshold < 1:
		return invalid("FaultThreshold", "must be 1 or more")
	case c.startTimeout <= 0:
		return invalid("StartTimeout", "must be above 0")
	case c.maxLocalFallbackDuration <= 0:
		return invalid("MaxLocalFallbackDuration", "must be above 0")
	case c.expiredPolicy > AllowAll:
		return invalid("LocalModeExpiredPolicy", "must be KeepLocalBreaker, RejectAll, or AllowAll")
	case c.startPolicy > StartRejecting:
		return invalid("StartPolicy", "must be StartWithProbe, StartClosed, or StartRejecting")
	case !c.localFallback && c.expiredPolicy == KeepLocalBreaker:
		return invalid("LocalModeExpiredPolicy", "must not be KeepLocalBreaker when LocalFallback is off")
	}
	return nil
}

func (c config) isSlow(d time.Duration) bool {
	return c.slowCallDurationSet && d >= c.slowCallDuration
}
