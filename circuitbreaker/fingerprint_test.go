package circuitbreaker

import (
	"slices"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/faultvanetest"
)

func fingerprintOf(t *testing.T, opts ...Option) string {
	t.Helper()
	b, err := New("payment-service", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return b.ConfigFingerprint()
}

func TestConfigFingerprint(t *testing.T) {
	t.Run("CFG-4 the fingerprint is a version and a hash of the canonical text of the shared values", func(t *testing.T) {
		c, err := newConfig("a", []Option{
			WithFailureThreshold(5), WithMinimumCalls(40), WithFailureRateThreshold(0.25),
			WithSlowCallDuration(2 * time.Second), WithSlowCallRateThreshold(0.75),
			WithWindowSize(20 * time.Second), WithWindowBuckets(4), WithOpenDuration(time.Minute),
			WithHalfOpenMaxProbes(3), WithHalfOpenSuccessThreshold(2), WithProbeTimeout(15 * time.Second), WithStateTTL(2 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		canonical := "v1|failure_threshold=5|minimum_calls=40|failure_rate=0.25|slow_call_rate=0.75|slow_call_duration=2000000000|window_size=20000000000|window_buckets=4|open_duration=60000000000|half_open_max_probes=3|half_open_success_threshold=2|probe_timeout=15000000000|state_ttl=7200000000000"
		if got := c.canonical(); got != canonical {
			t.Errorf("canonical() = %q, want %q", got, canonical)
		}
		if got, want := c.fingerprint(), "v1:d7c160a14c9efdf1"; got != want {
			t.Errorf("fingerprint() = %q, want %q", got, want)
		}
	})

	t.Run("CFG-4 the fingerprint of the default configuration is stable", func(t *testing.T) {
		if got, want := fingerprintOf(t), "v1:c0183f3821e11064"; got != want {
			t.Fatalf("ConfigFingerprint() = %q, want %q", got, want)
		}
	})

	t.Run("CFG-4 each shared value changes the fingerprint", func(t *testing.T) {
		slow := []Option{WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(0.5)}
		base := fingerprintOf(t, slow...)
		seen := map[string]string{base: "the base configuration"}
		changes := map[string]Option{
			"FailureThreshold":         WithFailureThreshold(5),
			"MinimumCalls":             WithMinimumCalls(21),
			"FailureRateThreshold":     WithFailureRateThreshold(0.6),
			"SlowCallDuration":         WithSlowCallDuration(2 * time.Second),
			"SlowCallRateThreshold":    WithSlowCallRateThreshold(0.6),
			"WindowSize":               WithWindowSize(20 * time.Second),
			"WindowBuckets":            WithWindowBuckets(5),
			"OpenDuration":             WithOpenDuration(31 * time.Second),
			"HalfOpenMaxProbes":        WithHalfOpenMaxProbes(2),
			"HalfOpenSuccessThreshold": WithHalfOpenSuccessThreshold(2),
			"ProbeTimeout":             WithProbeTimeout(31 * time.Second),
			"StateTTL":                 WithStateTTL(2 * time.Hour),
		}
		for name, change := range changes {
			got := fingerprintOf(t, append(slices.Clone(slow), change)...)
			if other, found := seen[got]; found {
				t.Errorf("%s gives the fingerprint of %s", name, other)
			}
			seen[got] = name
		}
	})

	t.Run("CFG-4 a local value does not change the fingerprint", func(t *testing.T) {
		store := NewMemoryStore(faultvanetest.NewClock(testStart))
		manual := func(c *config) { c.manualSync = true }
		base := fingerprintOf(t, WithStore(store), manual)
		local := map[string]Option{
			"Classifier":               WithClassifier(func(error) Result { return Success }),
			"Clock":                    WithClock(faultvanetest.NewClock(testStart)),
			"Logger":                   WithLogger(quiet),
			"Observer":                 WithObserver(&events{}),
			"StoreTimeout":             WithStoreTimeout(90 * time.Millisecond),
			"SyncInterval":             WithSyncInterval(time.Second),
			"EarlyReportInterval":      WithEarlyReportInterval(40 * time.Millisecond),
			"FaultThreshold":           WithFaultThreshold(4),
			"StartTimeout":             WithStartTimeout(2 * time.Second),
			"ProbeAttemptInterval":     WithProbeAttemptInterval(time.Second),
			"LocalFallback":            func(c *config) { c.localFallback, c.expiredPolicy = false, RejectAll },
			"MaxLocalFallbackDuration": WithMaxLocalFallbackDuration(time.Minute),
			"LocalModeExpiredPolicy":   WithLocalModeExpiredPolicy(AllowAll),
			"StartPolicy":              WithStartPolicy(StartClosed),
			"LocalFailureThreshold":    WithLocalFailureThreshold(3),
			"LocalMinimumCalls":        WithLocalMinimumCalls(3),
		}
		for name, option := range local {
			if got := fingerprintOf(t, WithStore(store), manual, option); got != base {
				t.Errorf("%s changed the fingerprint from %q to %q", name, base, got)
			}
		}
		if got := fingerprintOf(t); got != base {
			t.Errorf("a breaker without a store has the fingerprint %q, want %q", got, base)
		}
		other, err := New("a-different-name")
		if err != nil || other.ConfigFingerprint() != base {
			t.Errorf("a different name gives the fingerprint %q, %v, want %q", other.ConfigFingerprint(), err, base)
		}
	})
}
