package circuitbreaker

import (
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func TestDefaults(t *testing.T) {
	t.Run("CFG-2 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig("payment-service", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.thresholds != (Thresholds{MinimumCalls: 20, FailureRate: 0.5}) {
			t.Errorf("thresholds = %+v", c.thresholds)
		}
		if c.windowSize != 10*time.Second || c.windowBuckets != 10 {
			t.Errorf("window = %v, %d", c.windowSize, c.windowBuckets)
		}
		if c.openDuration != 30*time.Second || c.probeTimeout != 30*time.Second {
			t.Errorf("openDuration = %v, probeTimeout = %v", c.openDuration, c.probeTimeout)
		}
		if c.halfOpenMaxProbes != 1 || c.halfOpenSuccessThreshold != 1 {
			t.Errorf("halfOpenMaxProbes = %d, halfOpenSuccessThreshold = %d", c.halfOpenMaxProbes, c.halfOpenSuccessThreshold)
		}
		if c.isSlow(time.Hour) {
			t.Error("slow call detection is on")
		}
		if c.classifier(errors.New("x")) != Failure || c.clock != faultvane.SystemClock() {
			t.Error("classifier or clock is not the default")
		}
	})

	t.Run("CFG-2 the success threshold follows the probe limit", func(t *testing.T) {
		c, err := newConfig("a", []Option{WithHalfOpenMaxProbes(3)})
		if err != nil || c.halfOpenSuccessThreshold != 3 {
			t.Fatalf("halfOpenSuccessThreshold = %d, err = %v, want 3, nil", c.halfOpenSuccessThreshold, err)
		}
	})

	t.Run("CFG-2 an explicit success threshold stays", func(t *testing.T) {
		c, err := newConfig("a", []Option{WithHalfOpenMaxProbes(3), WithHalfOpenSuccessThreshold(5)})
		if err != nil || c.halfOpenSuccessThreshold != 5 {
			t.Fatalf("halfOpenSuccessThreshold = %d, err = %v, want 5, nil", c.halfOpenSuccessThreshold, err)
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("FD-5 CFG-1 each option sets its value", func(t *testing.T) {
		classifier := func(error) Result { return Ignored }
		clock := fixedClock{}
		c, err := newConfig("a", []Option{
			WithFailureRateThreshold(1),
			WithMinimumCalls(1),
			WithFailureThreshold(1),
			WithSlowCallDuration(time.Nanosecond),
			WithSlowCallRateThreshold(1),
			WithWindowSize(3 * time.Nanosecond),
			WithWindowBuckets(3),
			WithOpenDuration(time.Nanosecond),
			WithHalfOpenMaxProbes(2),
			WithHalfOpenSuccessThreshold(1),
			WithProbeTimeout(time.Nanosecond),
			WithClassifier(classifier),
			WithClock(clock),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.thresholds != (Thresholds{FailureThreshold: 1, MinimumCalls: 1, FailureRate: 1, SlowCallRate: 1}) {
			t.Errorf("thresholds = %+v", c.thresholds)
		}
		if c.windowSize != 3 || c.windowBuckets != 3 || c.openDuration != 1 || c.probeTimeout != 1 {
			t.Errorf("durations = %+v", c)
		}
		if c.halfOpenMaxProbes != 2 || c.halfOpenSuccessThreshold != 1 {
			t.Errorf("half-open = %d, %d", c.halfOpenMaxProbes, c.halfOpenSuccessThreshold)
		}
		if !c.isSlow(time.Nanosecond) || c.isSlow(0) {
			t.Error("slow call duration is not 1ns")
		}
		if c.classifier(nil) != Ignored || c.clock != clock {
			t.Error("classifier or clock is not set")
		}
	})
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return testStart }

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		opts   []Option
		option string
	}{
		{"empty name", "", nil, "name"},
		{"name with 65 characters", strings.Repeat("a", 65), nil, "name"},
		{"name with a space", "a b", nil, "name"},
		{"name with a colon", "a:b", nil, "name"},
		{"name with a brace", "{a}", nil, "name"},
		{"name with a slash", "a/b", nil, "name"},
		{"name with a character outside ASCII", "café", nil, "name"},
		{"failure rate 0", "a", []Option{WithFailureRateThreshold(0)}, "FailureRateThreshold"},
		{"failure rate below 0", "a", []Option{WithFailureRateThreshold(-0.1)}, "FailureRateThreshold"},
		{"failure rate above 1", "a", []Option{WithFailureRateThreshold(1.01)}, "FailureRateThreshold"},
		{"failure rate not a number", "a", []Option{WithFailureRateThreshold(math.NaN())}, "FailureRateThreshold"},
		{"minimum calls 0", "a", []Option{WithMinimumCalls(0)}, "MinimumCalls"},
		{"failure threshold 0", "a", []Option{WithFailureThreshold(0)}, "FailureThreshold"},
		{"failure threshold below 0", "a", []Option{WithFailureThreshold(-1)}, "FailureThreshold"},
		{"slow call duration without rate", "a", []Option{WithSlowCallDuration(time.Second)}, "SlowCallDuration and SlowCallRateThreshold"},
		{"slow call rate without duration", "a", []Option{WithSlowCallRateThreshold(0.5)}, "SlowCallDuration and SlowCallRateThreshold"},
		{"slow call duration 0", "a", []Option{WithSlowCallDuration(0), WithSlowCallRateThreshold(0.5)}, "SlowCallDuration"},
		{"slow call rate 0", "a", []Option{WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(0)}, "SlowCallRateThreshold"},
		{"slow call rate above 1", "a", []Option{WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(2)}, "SlowCallRateThreshold"},
		{"window buckets 0", "a", []Option{WithWindowBuckets(0)}, "WindowBuckets"},
		{"window size 0", "a", []Option{WithWindowSize(0)}, "WindowSize"},
		{"window size below 0", "a", []Option{WithWindowSize(-time.Second)}, "WindowSize"},
		{"window size shorter than the buckets", "a", []Option{WithWindowSize(9 * time.Nanosecond)}, "WindowSize"},
		{"open duration 0", "a", []Option{WithOpenDuration(0)}, "OpenDuration"},
		{"half-open probes 0", "a", []Option{WithHalfOpenMaxProbes(0), WithHalfOpenSuccessThreshold(1)}, "HalfOpenMaxProbes"},
		{"half-open success threshold 0", "a", []Option{WithHalfOpenSuccessThreshold(0)}, "HalfOpenSuccessThreshold"},
		{"probe timeout 0", "a", []Option{WithProbeTimeout(0)}, "ProbeTimeout"},
		{"nil classifier", "a", []Option{WithClassifier(nil)}, "Classifier"},
		{"nil clock", "a", []Option{WithClock(nil)}, "Clock"},
		{"probe attempt interval below 0", "a", []Option{WithProbeAttemptInterval(-time.Nanosecond)}, "ProbeAttemptInterval"},
		{"nil logger", "a", []Option{WithLogger(nil)}, "Logger"},
		{"nil observer", "a", []Option{WithObserver(nil)}, "Observer"},
		{"local failure threshold 0", "a", []Option{WithLocalFailureThreshold(0)}, "LocalFailureThreshold"},
		{"local minimum calls 0", "a", []Option{WithLocalMinimumCalls(0)}, "LocalMinimumCalls"},
		{"nil store", "a", []Option{WithStore(nil)}, "Store"},
		{"store timeout 0", "a", shared(WithStoreTimeout(0)), "StoreTimeout"},
		{"sync interval equal to the store timeout", "a", shared(WithSyncInterval(100 * time.Millisecond)), "SyncInterval"},
		{"early report interval 0", "a", shared(WithEarlyReportInterval(0)), "EarlyReportInterval"},
		{"early report interval longer than the sync interval", "a", shared(WithEarlyReportInterval(500*time.Millisecond + 1)), "EarlyReportInterval"},
		{"bucket shorter than the sync interval", "a", shared(WithWindowSize(5*time.Second - 10)), "WindowSize"},
		{"fault threshold 0", "a", shared(WithFaultThreshold(0)), "FaultThreshold"},
		{"start timeout 0", "a", shared(WithStartTimeout(0)), "StartTimeout"},
		{"maximum local fallback duration 0", "a", shared(WithMaxLocalFallbackDuration(0)), "MaxLocalFallbackDuration"},
		{"unknown expired policy", "a", shared(WithLocalModeExpiredPolicy(AllowAll + 1)), "LocalModeExpiredPolicy"},
		{"unknown start policy", "a", shared(WithStartPolicy(StartRejecting + 1)), "StartPolicy"},
		{"no local fallback with the policy KeepLocalBreaker", "a", shared(WithLocalFallback(false)), "LocalModeExpiredPolicy"},
	}
	for _, c := range cases {
		t.Run("CFG-3 TK-21 "+c.name, func(t *testing.T) {
			b, err := New(c.id, append([]Option{WithLogger(quiet)}, c.opts...)...)
			if b != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", b, err)
			}
			if want := "faultvane: invalid configuration: " + c.option + " "; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("err = %q, want the prefix %q", err, want)
			}
		})
	}

	valid := []struct {
		name string
		id   string
		opts []Option
	}{
		{"name with 64 characters", strings.Repeat("a", 64), nil},
		{"name with one character", "a", nil},
		{"name with each permitted type of character", "azAZ09._-", nil},
		{"failure rate 1", "a", []Option{WithFailureRateThreshold(1)}},
		{"smallest failure rate", "a", []Option{WithFailureRateThreshold(math.SmallestNonzeroFloat64)}},
		{"minimum calls 1", "a", []Option{WithMinimumCalls(1)}},
		{"failure threshold 1", "a", []Option{WithFailureThreshold(1)}},
		{"slow call values at their limits", "a", []Option{WithSlowCallDuration(time.Nanosecond), WithSlowCallRateThreshold(1)}},
		{"window size equal to the buckets", "a", []Option{WithWindowSize(10 * time.Nanosecond)}},
		{"one bucket", "a", []Option{WithWindowBuckets(1)}},
		{"shortest durations", "a", []Option{WithOpenDuration(time.Nanosecond), WithProbeTimeout(time.Nanosecond)}},
		{"probe attempt interval 0", "a", []Option{WithProbeAttemptInterval(0)}},
		{"store options without a store are not checked", "a", []Option{WithStoreTimeout(0), WithFaultThreshold(0)}},
		{"store with the defaults", "a", shared()},
		{"sync interval one step longer than the store timeout", "a", shared(WithSyncInterval(100*time.Millisecond + 1))},
		{"early report interval equal to the sync interval", "a", shared(WithEarlyReportInterval(500 * time.Millisecond))},
		{"bucket equal to the sync interval", "a", shared(WithWindowSize(5 * time.Second))},
		{"smallest store values", "a", shared(WithStoreTimeout(1), WithFaultThreshold(1), WithStartTimeout(1), WithMaxLocalFallbackDuration(1), WithEarlyReportInterval(1))},
		{"no local fallback with the policy RejectAll", "a", shared(WithLocalFallback(false), WithLocalModeExpiredPolicy(RejectAll))},
		{"each start policy and expired policy", "a", shared(WithStartPolicy(StartRejecting), WithLocalModeExpiredPolicy(AllowAll))},
		{"local thresholds at 1", "a", []Option{WithLocalFailureThreshold(1), WithLocalMinimumCalls(1)}},
	}
	for _, c := range valid {
		t.Run("CFG-3 valid: "+c.name, func(t *testing.T) {
			if _, err := New(c.id, append([]Option{WithLogger(quiet)}, c.opts...)...); err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
		})
	}
}

func shared(opts ...Option) []Option {
	manual := func(c *config) { c.manualSync = true }
	return append([]Option{WithStore(NewMemoryStore(fixedClock{})), manual}, opts...)
}

func TestStoreDefaults(t *testing.T) {
	t.Run("CFG-2 each store option has its documented default", func(t *testing.T) {
		c, err := newConfig("a", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.storeTimeout != 100*time.Millisecond || c.syncInterval != 500*time.Millisecond || c.earlyReportInterval != 50*time.Millisecond {
			t.Errorf("intervals = %v, %v, %v", c.storeTimeout, c.syncInterval, c.earlyReportInterval)
		}
		if c.faultThreshold != 3 || c.startTimeout != time.Second || c.stateTTL != time.Hour || c.probeAttemptInterval != 100*time.Millisecond {
			t.Errorf("faultThreshold = %d, startTimeout = %v, stateTTL = %v, probeAttemptInterval = %v", c.faultThreshold, c.startTimeout, c.stateTTL, c.probeAttemptInterval)
		}
		if !c.localFallback || c.maxLocalFallbackDuration != 5*time.Minute || c.expiredPolicy != KeepLocalBreaker || c.startPolicy != StartWithProbe {
			t.Errorf("local mode = %v, %v, %v, %v", c.localFallback, c.maxLocalFallbackDuration, c.expiredPolicy, c.startPolicy)
		}
		if c.localThresholds != c.thresholds || c.store != nil || c.observer != nil || c.logger != slog.Default() {
			t.Errorf("localThresholds = %+v, store = %v, observer = %v", c.localThresholds, c.store, c.observer)
		}
	})

	t.Run("CFG-1 each store option sets its value", func(t *testing.T) {
		store, observer := NewMemoryStore(fixedClock{}), &events{}
		c, err := newConfig("a", []Option{
			WithStore(store), WithStoreTimeout(2), WithSyncInterval(time.Second), WithEarlyReportInterval(3), WithFaultThreshold(4),
			WithStartTimeout(5), WithStateTTL(48 * time.Hour), WithProbeAttemptInterval(6), WithLocalFallback(false),
			WithMaxLocalFallbackDuration(7), WithLocalModeExpiredPolicy(RejectAll), WithStartPolicy(StartClosed),
			WithLocalFailureThreshold(8), WithLocalMinimumCalls(9), WithObserver(observer), WithLogger(quiet),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.store != store || c.storeTimeout != 2 || c.syncInterval != time.Second || c.earlyReportInterval != 3 || c.faultThreshold != 4 {
			t.Errorf("store values = %+v", c)
		}
		if c.startTimeout != 5 || c.stateTTL != 48*time.Hour || c.probeAttemptInterval != 6 || c.localFallback || c.maxLocalFallbackDuration != 7 {
			t.Errorf("values = %+v", c)
		}
		if c.expiredPolicy != RejectAll || c.startPolicy != StartClosed || c.observer != observer || c.logger != quiet {
			t.Errorf("policies = %+v", c)
		}
		if want := (Thresholds{FailureThreshold: 8, MinimumCalls: 9, FailureRate: 0.5}); c.localThresholds != want {
			t.Errorf("localThresholds = %+v, want %+v", c.localThresholds, want)
		}
	})

	t.Run("TTL-2 the state TTL is never shorter than two times the open duration plus the window size", func(t *testing.T) {
		c, err := newConfig("a", []Option{WithOpenDuration(time.Hour), WithWindowSize(10 * time.Minute)})
		if err != nil || c.stateTTL != 2*(time.Hour+10*time.Minute) {
			t.Fatalf("stateTTL = %v, err = %v", c.stateTTL, err)
		}
	})
}

func FuzzValidName(f *testing.F) {
	for _, seed := range []string{"", "a", "payment-service", "a b", "{a}", strings.Repeat("z", 64), strings.Repeat("z", 65), "A.b_c-9", "café"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		const permitted = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
		want := len(name) >= 1 && len(name) <= 64 && strings.Trim(name, permitted) == ""
		if got := validName(name); got != want {
			t.Fatalf("validName(%q) = %v, want %v", name, got, want)
		}
	})
}
