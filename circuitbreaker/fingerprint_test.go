package circuitbreaker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
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

func mismatchEvents(e *events) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var found []Event
	for _, event := range e.list {
		if event.Kind == ConfigMismatchChanged {
			found = append(found, event)
		}
	}
	return found
}

func (f *fleet) assertMismatch(want ...bool) {
	f.t.Helper()
	for i, b := range f.breakers {
		if got := b.Stats().ConfigMismatch; got != want[i] {
			f.t.Fatalf("instance %d: ConfigMismatch = %v, want %v", i, got, want[i])
		}
	}
}

func TestConfigMismatch(t *testing.T) {
	different := WithFailureThreshold(6)

	t.Run("FD-8 instances with one configuration find no difference", func(t *testing.T) {
		f := newFleet(t, 3)
		for round := range 3 {
			f.fail(round, 1)
			f.sync()
		}
		f.openAll()
		f.assertMismatch(false, false, false)
		for i, e := range f.events {
			if len(mismatchEvents(e)) != 0 {
				t.Fatalf("instance %d got a mismatch event", i)
			}
		}
	})

	t.Run("FD-8 OB-6 an instance finds the different configuration of the last writer and reports it one time", func(t *testing.T) {
		f := newFleet(t, 1)
		other := f.add(different)
		f.assertMismatch(false, false)
		f.fail(0, 1)
		f.sync(0)
		f.assertMismatch(false, false)
		for range 3 {
			f.sync(1)
		}
		f.assertMismatch(false, true)
		want := []Event{{Kind: ConfigMismatchChanged, Breaker: sharedName, Mode: Distributed, ConfigMismatch: true}}
		if got := mismatchEvents(f.events[1]); !slices.Equal(got, want) {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
		if f.stored().Fingerprint != f.breakers[0].ConfigFingerprint() || other.ConfigFingerprint() == f.breakers[0].ConfigFingerprint() {
			t.Fatalf("stored fingerprint = %q, instances have %q and %q", f.stored().Fingerprint, f.breakers[0].ConfigFingerprint(), other.ConfigFingerprint())
		}
	})

	t.Run("FD-8 the first writer finds the difference when the different instance writes", func(t *testing.T) {
		f := newFleet(t, 1)
		f.add(different)
		f.fail(0, 1)
		f.sync(0)
		f.fail(1, 1)
		f.sync(1)
		f.assertMismatch(false, true)
		if f.stored().Fingerprint != f.breakers[1].ConfigFingerprint() {
			t.Fatal("the store does not have the fingerprint of the last writer")
		}
		f.sync(0)
		f.assertMismatch(true, true)
	})

	t.Run("FD-8 a new instance finds the difference at its start", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		late := f.add(different)
		if !late.Stats().ConfigMismatch {
			t.Fatal("the new instance found no difference")
		}
		want := []Event{{Kind: ConfigMismatchChanged, Breaker: sharedName, From: Open, To: Open, Generation: 1, Mode: Distributed, ConfigMismatch: true}}
		if got := mismatchEvents(f.events[1]); !slices.Equal(got, want) {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	})

	t.Run("FD-8 an instance finds the difference in the response of a probe request", func(t *testing.T) {
		f := newFleet(t, 1)
		f.add(different)
		f.openAll()
		f.assertMismatch(false, true)
		f.clock.Advance(testOpenDuration)
		probe, err := f.breakers[1].Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertRejected(t, f.breakers[0], ErrProbeNotPermitted)
		f.assertMismatch(true, true)
		probe.Success()
		f.assertStored(Closed, 3)
	})

	t.Run("FD-8 OB-6 the difference ends when the last writer has the equal configuration", func(t *testing.T) {
		f := newFleet(t, 1)
		f.add(different)
		f.fail(0, 1)
		f.sync(0)
		f.sync(1)
		f.assertMismatch(false, true)
		f.fail(1, 1)
		f.sync(1)
		f.assertMismatch(false, true)
		for range 3 {
			f.sync(1)
		}
		f.assertMismatch(false, false)
		want := []Event{
			{Kind: ConfigMismatchChanged, Breaker: sharedName, Mode: Distributed, ConfigMismatch: true},
			{Kind: ConfigMismatchChanged, Breaker: sharedName, Mode: Distributed, ConfigMismatch: false},
		}
		if got := mismatchEvents(f.events[1]); !slices.Equal(got, want) {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	})

	t.Run("FD-8 DS-9 a stored state without a fingerprint is not a difference", func(t *testing.T) {
		clock := faultvanetest.NewClock(testStart)
		f := &fleet{t: t, clock: clock, memory: NewMemoryStore(clock)}
		f.memory.records[sharedName] = Record{State: Open, Generation: 4, TransitionAt: testStart, OpenDuration: testOpenDuration}
		f.add()
		f.sync()
		f.assertAll(Open, Distributed)
		f.assertMismatch(false)
		f.add(different)
		f.memory.records[sharedName] = Record{Fingerprint: f.breakers[0].ConfigFingerprint()}
		f.sync()
		f.assertMismatch(false, true)
		f.memory.Delete(sharedName)
		f.sync()
		f.assertMismatch(false, false)
		if got := len(mismatchEvents(f.events[1])); got != 2 {
			t.Fatalf("mismatch events = %d, want 2", got)
		}
	})

	t.Run("FD-8 PF-1 PF-2 a difference changes no decision and adds no store operation and no allocation to a call", func(t *testing.T) {
		f := newFleet(t, 1)
		b := f.add(different)
		f.fail(0, 1)
		f.sync()
		f.assertMismatch(false, true)
		before := f.stores[1].operations.Load()
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = b.Do(ctx, call) }); allocations != 0 {
			t.Errorf("%v allocations for each call, want 0", allocations)
		}
		if got := f.stores[1].operations.Load(); got != before {
			t.Errorf("store operations went from %d to %d during calls", before, got)
		}
		f.fail(1, 5)
		f.sync()
		f.sync()
		f.assertAll(Open, Distributed)
		f.assertStored(Open, 1)
		f.clock.Advance(testOpenDuration)
		runTimes(t, b, 1, nil)
		f.assertStored(Closed, 3)
	})

	t.Run("FD-8 the difference stays while the store is not available", func(t *testing.T) {
		f := newFleet(t, 1)
		f.add(different)
		f.fail(0, 1)
		f.sync()
		f.disconnect(1)
		if f.breakers[1].Mode() != Local {
			t.Fatal("the instance is not in local mode")
		}
		f.assertMismatch(false, true)
		f.connect(1)
		f.sync(1)
		f.assertMismatch(false, true)
		if got := len(mismatchEvents(f.events[1])); got != 1 {
			t.Fatalf("mismatch events = %d, want 1", got)
		}
	})

	t.Run("OB-6 the breaker logs one record when the difference starts and one when it ends", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
		f := newFleet(t, 1)
		b := f.add(different, WithLogger(logger))
		f.fail(0, 1)
		f.sync(0)
		f.sync(1)
		f.sync(1)
		f.fail(1, 1)
		f.sync(1)
		f.sync(1)
		f.sync(1)
		type line struct {
			Level, Msg, Breaker, Mode string
			Fingerprint               string
			Stored                    string `json:"stored_fingerprint"`
		}
		var lines []line
		for _, raw := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			var l line
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, l)
		}
		own, first := b.ConfigFingerprint(), f.breakers[0].ConfigFingerprint()
		want := []line{
			{"WARN", "circuit breaker configuration is different from the configuration of the last writer", sharedName, "distributed", own, first},
			{"INFO", "circuit breaker configuration is equal to the configuration of the last writer again", sharedName, "distributed", own, own},
		}
		if !slices.Equal(lines, want) {
			t.Fatalf("log lines:\n%+v\nwant:\n%+v", lines, want)
		}
	})
}
