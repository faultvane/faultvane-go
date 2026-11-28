package circuitbreaker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func TestLocalMode(t *testing.T) {
	t.Run("DG-1 DG-2 AT-3 an instance changes to local mode after the fault threshold and continues to work", func(t *testing.T) {
		f := newFleet(t, 1)
		b := f.breakers[0]
		f.stores[0].down.Store(true)
		f.sync()
		f.sync()
		if b.Mode() != Distributed {
			t.Fatalf("Mode() = %v after 2 faults, want distributed", b.Mode())
		}
		f.sync()
		if b.Mode() != Local || b.State() != Closed {
			t.Fatalf("%v in %v mode after 3 faults, want closed in local mode", b.State(), b.Mode())
		}
		runTimes(t, b, 20, nil)
		f.fail(0, 4)
		if b.State() != Closed {
			t.Fatalf("State() = %v, want closed", b.State())
		}
		f.fail(0, 1)
		if b.State() != Open {
			t.Fatalf("State() = %v, want open from the local window", b.State())
		}
		assertRejected(t, b, ErrCircuitOpen)
		if s := b.Stats(); s.StoreErrors != 3 || s.LocalModeEntries != 1 || s.Mode != Local {
			t.Fatalf("stats = %+v", s)
		}
	})

	t.Run("DG-2 the local window keeps the calls from before the fault", func(t *testing.T) {
		f := newFleet(t, 1)
		f.fail(0, 4)
		f.disconnect(0)
		f.fail(0, 1)
		if f.breakers[0].State() != Open {
			t.Fatalf("State() = %v, want open", f.breakers[0].State())
		}
	})

	t.Run("ERR-3 TK-32 TK-33 a store fault never fails a call", func(t *testing.T) {
		f := newFleet(t, 1, WithFailureThreshold(1000))
		f.stores[0].down.Store(true)
		for range 10 {
			f.sync()
			if err := run(f.breakers[0], nil); err != nil {
				t.Fatalf("Do() = %v, want nil", err)
			}
			if err := run(f.breakers[0], errDependency); err != errDependency {
				t.Fatalf("Do() = %v, want the error of the dependency", err)
			}
		}
		f.openAllLocal()
		if err := run(f.breakers[0], nil); errors.Is(err, faultvane.ErrStoreUnavailable) || err != ErrCircuitOpen {
			t.Fatalf("Do() = %v, want ErrCircuitOpen", err)
		}
	})

	t.Run("DG-3 DG-4 RF-2 RF-4 a store that does not answer delays no call", func(t *testing.T) {
		f := newFleet(t, 1, WithStoreTimeout(10*time.Millisecond))
		f.stores[0].hang.Store(true)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 3 {
				f.sync()
			}
		}()
		runTimes(t, f.breakers[0], 100, nil)
		<-done
		if f.breakers[0].Mode() != Local {
			t.Fatalf("Mode() = %v, want local", f.breakers[0].Mode())
		}
	})

	t.Run("DG-2 an open circuit stays open in local mode until the open duration ends, then one local probe runs", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(10 * time.Second)
		f.disconnect(0)
		b := f.breakers[0]
		if b.State() != Open || b.Mode() != Local {
			t.Fatalf("%v in %v mode, want open in local mode", b.State(), b.Mode())
		}
		f.clock.Advance(testOpenDuration - 10*time.Second - time.Nanosecond)
		assertRejected(t, b, ErrCircuitOpen)
		f.clock.Advance(time.Nanosecond)
		probe, err := b.Allow(context.Background())
		if err != nil || b.State() != HalfOpen {
			t.Fatalf("Allow() error = %v in %v, want a local probe in half_open", err, b.State())
		}
		assertRejected(t, b, ErrProbeNotPermitted)
		probe.Success()
		if b.State() != Closed {
			t.Fatalf("State() = %v, want closed", b.State())
		}
		f.assertStored(Open, 1)
	})

	t.Run("DG-2 a half-open circuit gives a local probe in local mode", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		shared, err := f.breakers[0].Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		f.sync(1)
		f.disconnect(1)
		b := f.breakers[1]
		if b.State() != HalfOpen || b.Mode() != Local {
			t.Fatalf("%v in %v mode, want half_open in local mode", b.State(), b.Mode())
		}
		runTimes(t, b, 1, errDependency)
		if b.State() != Open {
			t.Fatalf("State() = %v, want open after a failed local probe", b.State())
		}
		shared.Success()
		f.assertStored(Closed, 3)
	})

	t.Run("DG-6 HO-1 each instance in local mode has its own probe", func(t *testing.T) {
		f := newFleet(t, 3)
		f.openAll()
		f.disconnect()
		f.clock.Advance(testOpenDuration)
		for _, b := range f.breakers {
			if _, err := b.Allow(context.Background()); err != nil {
				t.Fatalf("Allow() error = %v, want a local probe", err)
			}
			assertRejected(t, b, ErrProbeNotPermitted)
		}
	})

	t.Run("DG-2 the local thresholds replace the normal thresholds in local mode", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalFailureThreshold(2), WithLocalMinimumCalls(3), WithFailureRateThreshold(0.5))
		f.fail(0, 4)
		if f.breakers[0].State() != Closed {
			t.Fatalf("State() = %v, want closed in distributed mode", f.breakers[0].State())
		}
		f.sync()
		f.disconnect(0)
		f.clock.Advance(10 * time.Second)
		f.fail(0, 1)
		if f.breakers[0].State() != Closed {
			t.Fatalf("State() = %v, want closed below the local threshold", f.breakers[0].State())
		}
		f.fail(0, 1)
		if f.breakers[0].State() != Open {
			t.Fatalf("State() = %v, want open at the local threshold", f.breakers[0].State())
		}
	})

	t.Run("DG-2 the local minimum number of calls applies in local mode", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalMinimumCalls(3), WithFailureRateThreshold(1), WithFailureThreshold(100))
		f.disconnect(0)
		f.fail(0, 2)
		if f.breakers[0].State() != Closed {
			t.Fatalf("State() = %v, want closed below the local minimum", f.breakers[0].State())
		}
		f.fail(0, 1)
		if f.breakers[0].State() != Open {
			t.Fatalf("State() = %v, want open", f.breakers[0].State())
		}
	})
}

func TestFaultCount(t *testing.T) {
	t.Run("DG-1 a successful synchronization starts the fault count again", func(t *testing.T) {
		f := newFleet(t, 1)
		for range 5 {
			f.stores[0].down.Store(true)
			f.sync()
			f.sync()
			f.stores[0].down.Store(false)
			f.sync()
		}
		if f.breakers[0].Mode() != Distributed {
			t.Fatalf("Mode() = %v, want distributed", f.breakers[0].Mode())
		}
	})

	t.Run("DG-1 a successful probe request starts the fault count again", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		if _, err := f.breakers[1].Allow(context.Background()); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			f.stores[0].down.Store(true)
			f.sync(0)
			f.sync(0)
			f.stores[0].down.Store(false)
			assertRejected(t, f.breakers[0], ErrProbeNotPermitted)
		}
		if f.breakers[0].Mode() != Distributed {
			t.Fatalf("Mode() = %v, want distributed", f.breakers[0].Mode())
		}
	})

	t.Run("DG-1 a successful probe result starts the fault count again", func(t *testing.T) {
		f := newFleet(t, 1, WithHalfOpenSuccessThreshold(100))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		for range 5 {
			probe, err := f.breakers[0].Allow(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			f.stores[0].down.Store(true)
			f.sync()
			f.sync()
			f.stores[0].down.Store(false)
			probe.Success()
		}
		if f.breakers[0].Mode() != Distributed {
			t.Fatalf("Mode() = %v, want distributed", f.breakers[0].Mode())
		}
	})
}

func TestLocalModeLimit(t *testing.T) {
	expire := func(f *fleet) {
		f.disconnect(0)
		f.clock.Advance(5*time.Minute - time.Nanosecond)
		f.sync()
		if f.breakers[0].Stats().LocalModeExpired {
			f.t.Fatal("local mode expired before the maximum time")
		}
		f.clock.Advance(time.Nanosecond)
		f.sync()
	}

	t.Run("DG-5 the default policy keeps the local breaker and reports the limit one time", func(t *testing.T) {
		f := newFleet(t, 1)
		expire(f)
		f.sync()
		b := f.breakers[0]
		if !b.Stats().LocalModeExpired || f.events[0].count(LocalModeExpired) != 1 {
			t.Fatalf("expired = %v, events = %d, want true and 1", b.Stats().LocalModeExpired, f.events[0].count(LocalModeExpired))
		}
		f.fail(0, 5)
		if b.State() != Open || b.Mode() != Local {
			t.Fatalf("%v in %v mode, want the local breaker to open", b.State(), b.Mode())
		}
	})

	t.Run("DG-5 the policy RejectAll rejects all calls", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalModeExpiredPolicy(RejectAll))
		runTimes(t, f.breakers[0], 1, nil)
		expire(f)
		f.clock.Advance(24 * time.Hour)
		assertRejected(t, f.breakers[0], ErrCircuitOpen)
		if f.breakers[0].State() != Open {
			t.Fatalf("State() = %v, want open", f.breakers[0].State())
		}
	})

	t.Run("DG-5 the policy AllowAll permits all calls and records nothing", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalModeExpiredPolicy(AllowAll))
		expire(f)
		f.fail(0, 50)
		b := f.breakers[0]
		if b.State() != Closed || b.Stats().Failures != 0 {
			t.Fatalf("%v with %d failures, want closed with 0", b.State(), b.Stats().Failures)
		}
		permit, err := b.Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		permit.Failure()
	})

	t.Run("DG-5 a local probe request finds the policy AllowAll", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalModeExpiredPolicy(AllowAll))
		f.openAll()
		expire(f)
		permit, err := f.breakers[0].acquireLocalProbe(f.clock.Now())
		if err != nil || permit.breaker != nil {
			t.Fatalf("acquireLocalProbe() = %+v, %v, want an empty permit", permit, err)
		}
	})

	t.Run("DG-5 an open circuit with the policy AllowAll permits calls after the limit", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalModeExpiredPolicy(AllowAll))
		f.openAll()
		expire(f)
		runTimes(t, f.breakers[0], 3, errDependency)
	})

	t.Run("DG-5 CFG-1 without local fallback the policy applies at the fault threshold", func(t *testing.T) {
		f := newFleet(t, 1, WithLocalFallback(false), WithLocalModeExpiredPolicy(RejectAll))
		f.disconnect(0)
		assertRejected(t, f.breakers[0], ErrCircuitOpen)
		if s := f.breakers[0].Stats(); !s.LocalModeExpired || s.Mode != Local {
			t.Fatalf("stats = %+v", s)
		}
		f.clock.Advance(time.Hour)
		f.sync()
		if got := f.events[0].count(LocalModeExpired); got != 1 {
			t.Fatalf("LocalModeExpired events = %d, want 1", got)
		}
	})

	t.Run("RC-4 a recovery ends each policy", func(t *testing.T) {
		for _, policy := range []ExpiredPolicy{KeepLocalBreaker, RejectAll, AllowAll} {
			f := newFleet(t, 1, WithLocalModeExpiredPolicy(policy))
			expire(f)
			f.connect(0)
			f.sync()
			b := f.breakers[0]
			if b.Mode() != Distributed || b.State() != Closed || b.Stats().LocalModeExpired {
				t.Fatalf("policy %d: %v in %v mode, expired = %v", policy, b.State(), b.Mode(), b.Stats().LocalModeExpired)
			}
			f.fail(0, 5)
			f.sync()
			f.assertStored(Open, 1)
		}
	})
}

func TestStartWithoutStore(t *testing.T) {
	start := func(t *testing.T, opts ...Option) (*fleet, *Breaker) {
		f := newFleet(t, 0, opts...)
		store := &testStore{Store: f.memory}
		store.down.Store(true)
		return f, f.addWithStore(store)
	}

	t.Run("DG-7 RC-6 the default start policy starts with one probe", func(t *testing.T) {
		f, b := start(t)
		if b.State() != HalfOpen || b.Mode() != Local {
			t.Fatalf("%v in %v mode, want half_open in local mode", b.State(), b.Mode())
		}
		probe, err := b.Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertRejected(t, b, ErrProbeNotPermitted)
		probe.Success()
		if b.State() != Closed {
			t.Fatalf("State() = %v, want closed", b.State())
		}
		if s := b.Stats(); s.LocalModeEntries != 1 || s.StoreErrors != 1 {
			t.Fatalf("stats = %+v", s)
		}
		if kinds := f.events[0].kinds(); !slices.Equal(kinds[:2], []EventKind{StoreFault, ModeChanged}) {
			t.Fatalf("events = %v", kinds)
		}
	})

	t.Run("DG-7 a failed start probe opens the local circuit", func(t *testing.T) {
		_, b := start(t)
		runTimes(t, b, 1, errDependency)
		if b.State() != Open {
			t.Fatalf("State() = %v, want open", b.State())
		}
	})

	t.Run("DG-7 the policy StartClosed starts closed", func(t *testing.T) {
		_, b := start(t, WithStartPolicy(StartClosed))
		if b.State() != Closed || b.Mode() != Local {
			t.Fatalf("%v in %v mode, want closed in local mode", b.State(), b.Mode())
		}
		runTimes(t, b, 5, errDependency)
		if b.State() != Open {
			t.Fatalf("State() = %v, want open", b.State())
		}
	})

	t.Run("DG-7 the policy StartRejecting rejects until the first read", func(t *testing.T) {
		f, b := start(t, WithStartPolicy(StartRejecting))
		f.clock.Advance(24 * time.Hour)
		assertRejected(t, b, ErrCircuitOpen)
		f.connect(0)
		f.sync()
		if b.State() != Closed || b.Mode() != Distributed {
			t.Fatalf("%v in %v mode, want closed in distributed mode", b.State(), b.Mode())
		}
		runTimes(t, b, 1, nil)
	})

	t.Run("RC-6 an instance that started without the store adopts the state of the store", func(t *testing.T) {
		f, b := start(t)
		f.add()
		f.fail(1, 5)
		f.sync(1)
		runTimes(t, b, 1, nil)
		if b.State() != Closed {
			t.Fatalf("State() = %v, want a local closed state", b.State())
		}
		f.connect(0)
		f.sync(0)
		if b.State() != Open || b.Mode() != Distributed {
			t.Fatalf("%v in %v mode, want open in distributed mode", b.State(), b.Mode())
		}
		f.assertStored(Open, 1)
	})

	t.Run("RF-2 the first read waits only for the start timeout", func(t *testing.T) {
		f := newFleet(t, 0, WithStartTimeout(10*time.Millisecond))
		store := &testStore{Store: f.memory}
		store.hang.Store(true)
		if b := f.addWithStore(store); b.Mode() != Local {
			t.Fatalf("Mode() = %v, want local", b.Mode())
		}
	})
}

func TestReconciliation(t *testing.T) {
	t.Run("RC-1 RC-3 AT-4 TK-34 a newer state in the store replaces a local state", func(t *testing.T) {
		f := newFleet(t, 2)
		f.disconnect(0)
		f.fail(1, 5)
		f.sync(1)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[1], 1, errDependency)
		f.assertStored(Open, 3)
		runTimes(t, f.breakers[0], 3, nil)
		f.connect(0)
		before := f.stores[0].operations.Load()
		f.sync(0)
		if got := f.stores[0].operations.Load(); got != before+1 {
			t.Fatalf("the reconciliation made %d store operations, want 1 read", got-before)
		}
		if b := f.breakers[0]; b.State() != Open || b.Mode() != Distributed || b.current.Load().generation != 3 {
			t.Fatalf("%v in %v mode, generation %d, want open, distributed, 3", b.State(), b.Mode(), b.current.Load().generation)
		}
		f.assertStored(Open, 3)
		f.sync(0)
		if len(f.stored().Buckets) != 0 || len(f.stores[0].restores) != 0 {
			t.Fatal("the instance wrote to the store during reconciliation")
		}
	})

	t.Run("RC-2 RC-5 a local open state with too little evidence does not open the shared circuit", func(t *testing.T) {
		f := newFleet(t, 2, WithLocalFailureThreshold(2))
		f.disconnect(0)
		f.fail(0, 2)
		if f.breakers[0].State() != Open {
			t.Fatalf("State() = %v, want a local open state", f.breakers[0].State())
		}
		f.connect(0)
		f.sync(0)
		f.assertStored(Closed, 0)
		if b := f.breakers[0]; b.State() != Closed || b.Mode() != Distributed {
			t.Fatalf("%v in %v mode, want closed in distributed mode", b.State(), b.Mode())
		}
		if bucket := f.stored().Buckets[0]; bucket.Calls != 2 || bucket.Failures != 2 {
			t.Fatalf("shared window = %+v, want the 2 local failures", bucket)
		}
	})

	t.Run("RC-4 the evidence of all instances opens the shared circuit after recovery", func(t *testing.T) {
		f := newFleet(t, 3, WithLocalFailureThreshold(2))
		f.disconnect()
		for i := range f.breakers {
			f.fail(i, 2)
		}
		f.connect(0, 1, 2)
		f.sync()
		f.sync()
		f.assertAll(Open, Distributed)
		f.assertStored(Open, 1)
	})

	t.Run("RC-3 a local closed state does not close an open shared circuit", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.disconnect(0)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		if f.breakers[0].State() != Closed {
			t.Fatalf("State() = %v, want a local closed state", f.breakers[0].State())
		}
		runTimes(t, f.breakers[0], 3, nil)
		f.connect(0)
		before := f.stores[0].operations.Load()
		f.sync(0)
		f.assertStored(Open, 1)
		if b := f.breakers[0]; b.State() != Open || b.Mode() != Distributed {
			t.Fatalf("%v in %v mode, want open in distributed mode", b.State(), b.Mode())
		}
		if got := f.stores[0].operations.Load(); got != before+1 {
			t.Fatalf("the reconciliation made %d store operations, want 1 read", got-before)
		}
	})

	t.Run("RC-4 AT-6 instances with different local states converge to the state of the store", func(t *testing.T) {
		f := newFleet(t, 5, WithLocalFailureThreshold(1))
		f.disconnect()
		f.fail(0, 1)
		f.fail(2, 1)
		runTimes(t, f.breakers[1], 3, nil)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[2], 1, errDependency)
		states := map[State]bool{}
		for _, b := range f.breakers {
			states[b.State()] = true
		}
		if len(states) < 2 {
			t.Fatalf("local states = %v, want different states", states)
		}
		f.connect(0, 1, 2, 3, 4)
		f.sync()
		f.sync()
		first := f.breakers[0]
		for i, b := range f.breakers {
			if b.Mode() != Distributed || b.State() != first.State() || b.current.Load().generation != first.current.Load().generation {
				t.Fatalf("instance %d: %v generation %d in %v mode, instance 0 has %v generation %d", i, b.State(), b.current.Load().generation, b.Mode(), first.State(), first.current.Load().generation)
			}
		}
		stored := f.stored()
		if stored.State != first.State() || stored.Generation != first.current.Load().generation {
			t.Fatalf("store = %v generation %d", stored.State, stored.Generation)
		}
	})

	t.Run("RC-5 counts that left the window are not reported after a long local mode", func(t *testing.T) {
		f := newFleet(t, 1, WithFailureThreshold(100), WithLocalFailureThreshold(100))
		f.disconnect(0)
		f.fail(0, 4)
		f.clock.Advance(time.Hour)
		f.fail(0, 1)
		f.connect(0)
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.Calls != 1 || bucket.Failures != 1 {
			t.Fatalf("shared window = %+v, want only the 1 call that is still in the local window", bucket)
		}
	})

	t.Run("RC-1 a recovery without pending counts sends no report", func(t *testing.T) {
		f := newFleet(t, 1)
		f.disconnect(0)
		f.connect(0)
		before := f.stores[0].operations.Load()
		f.sync()
		if got := f.stores[0].operations.Load(); got != before+1 || len(f.stored().Buckets) != 0 {
			t.Fatalf("store operations = %d, want %d", got, before+1)
		}
		if s := f.breakers[0].Stats(); s.Reconciliations != 1 || s.Mode != Distributed {
			t.Fatalf("stats = %+v", s)
		}
	})

	t.Run("RF-3 a recovery that fails keeps local mode and keeps the pending counts", func(t *testing.T) {
		f := newFleet(t, 1)
		f.disconnect(0)
		f.fail(0, 4)
		f.sync()
		f.sync()
		if f.breakers[0].Mode() != Local {
			t.Fatalf("Mode() = %v, want local", f.breakers[0].Mode())
		}
		f.connect(0)
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.Failures != 4 {
			t.Fatalf("shared window has %d failures, want 4", bucket.Failures)
		}
	})

	t.Run("RC-7 RC-4 the counts of a failed reconciliation report open the shared circuit at the next reconciliation", func(t *testing.T) {
		f := newFleet(t, 0)
		store := &failsReports{testStore: &testStore{Store: f.memory}}
		f.addWithStore(store.testStore, WithStore(store))
		f.add()
		f.disconnect()
		f.fail(0, 3)
		f.fail(1, 3)
		f.connect(0, 1)
		store.failReport = true
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.Failures != 3 || f.breakers[0].Mode() != Local {
			t.Fatalf("shared window has %d failures and instance 0 is in %v mode, want 3 and local", bucket.Failures, f.breakers[0].Mode())
		}
		store.failReport = false
		f.sync()
		f.assertStored(Open, 1)
		f.assertAll(Open, Distributed)
	})

	t.Run("RC-7 RC-5 kept counts that left the window are not sent", func(t *testing.T) {
		f := newFleet(t, 0, WithFailureThreshold(100), WithLocalFailureThreshold(100))
		store := &failsReports{testStore: &testStore{Store: f.memory}}
		f.addWithStore(store.testStore, WithStore(store))
		f.disconnect()
		f.fail(0, 4)
		f.connect(0)
		store.failReport = true
		f.sync()
		store.failReport = false
		f.clock.Advance(time.Hour)
		f.fail(0, 1)
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.Calls != 1 || bucket.Failures != 1 {
			t.Fatalf("shared window = %+v, want only the 1 call that is still in the local window", bucket)
		}
	})

	t.Run("RC-7 GEN-3 kept counts are not sent after a transition in the store", func(t *testing.T) {
		f := newFleet(t, 0)
		store := &failsReports{testStore: &testStore{Store: f.memory}}
		f.addWithStore(store.testStore, WithStore(store))
		f.add()
		f.disconnect(0)
		f.fail(0, 4)
		f.connect(0)
		store.failReport = true
		f.sync(0)
		store.failReport = false
		f.openAllFrom(1)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[1], 1, nil)
		f.assertStored(Closed, 3)
		f.sync(0)
		f.sync(0)
		if stored := f.stored(); stored.State != Closed || stored.Generation != 3 || len(stored.Buckets) != 0 {
			t.Fatalf("store = %+v, want closed generation 3 with an empty window", stored)
		}
		f.assertAll(Closed, Distributed)
	})

	t.Run("RC-7 FD-10 a reconciliation report that the store applied without a response counts one time", func(t *testing.T) {
		f := newFleet(t, 0, WithFailureThreshold(100), WithLocalFailureThreshold(100))
		store := &failsReports{testStore: &testStore{Store: f.memory}, applyReport: true}
		f.addWithStore(store.testStore, WithStore(store))
		f.disconnect()
		f.fail(0, 4)
		f.connect(0)
		store.failReport = true
		f.sync()
		store.failReport = false
		f.sync()
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.Calls != 4 || bucket.Failures != 4 || f.breakers[0].Mode() != Distributed {
			t.Fatalf("shared window = %+v in %v mode, want the 4 failures one time in distributed mode", bucket, f.breakers[0].Mode())
		}
	})
}

type failsReports struct {
	*testStore
	failReport      bool
	applyReport     bool
	withoutIdentity bool
}

func (s *failsReports) Report(ctx context.Context, name string, report Report) (Snapshot, error) {
	if s.withoutIdentity {
		report.Instance, report.Sequence = 0, 0
	}
	if s.failReport {
		if s.applyReport {
			_, _ = s.testStore.Report(ctx, name, report)
		}
		return Snapshot{}, errStoreDown
	}
	return s.testStore.Report(ctx, name, report)
}

func TestDataLoss(t *testing.T) {
	t.Run("RF-5 an instance in distributed mode restores a state that the store lost", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.memory.Delete(sharedName)
		f.assertStored(Closed, 0)
		f.sync(0)
		f.assertStored(Open, 1)
		if restores := f.stores[0].restores; len(restores) != 1 || restores[0].State != Open || restores[0].Generation != 1 {
			t.Fatalf("restores = %+v", restores)
		}
		f.sync(1)
		f.assertAll(Open, Distributed)
		if len(f.stores[1].restores) != 0 {
			t.Fatal("the second instance sent a restore for a state that the store has")
		}
	})

	t.Run("RF-5 RC-2 the restored open duration continues", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(10 * time.Second)
		f.memory.Delete(sharedName)
		f.sync()
		f.clock.Advance(testOpenDuration - 10*time.Second - time.Nanosecond)
		assertRejected(t, f.breakers[0], ErrCircuitOpen)
		f.clock.Advance(time.Nanosecond)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(Closed, 3)
	})

	t.Run("RF-5 the highest generation wins when many instances restore", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.disconnect(1)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(Closed, 3)
		f.memory.Delete(sharedName)
		f.connect(1)
		f.sync(1)
		f.assertStored(Open, 1)
		f.sync(0)
		f.assertStored(Closed, 3)
		f.sync(1)
		f.assertAll(Closed, Distributed)
	})

	t.Run("RF-5 an instance in local mode restores a lost state at recovery", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.disconnect(0)
		f.memory.Delete(sharedName)
		f.connect(0)
		f.sync()
		f.assertStored(Open, 1)
		f.assertAll(Open, Distributed)
	})

	t.Run("RF-3 a restore that fails counts as a store fault", func(t *testing.T) {
		f := newFleet(t, 0)
		store := &lossyStore{testStore: &testStore{Store: f.memory}}
		b := f.addWithStore(store.testStore, WithStore(store))
		f.fail(0, 5)
		f.sync()
		f.memory.Delete(sharedName)
		store.failRestore = true
		f.sync()
		if s := b.Stats(); s.StoreErrors != 1 || b.State() != Open {
			t.Fatalf("stats = %+v", s)
		}
		f.disconnect(0)
		f.connect(0)
		f.sync()
		if b.Mode() != Local {
			t.Fatalf("Mode() = %v, want local while the restore fails", b.Mode())
		}
	})

	t.Run("RF-3 a report that fails during recovery keeps local mode", func(t *testing.T) {
		f := newFleet(t, 0)
		store := &failsReports{testStore: &testStore{Store: f.memory}}
		b := f.addWithStore(store.testStore, WithStore(store))
		f.disconnect(0)
		f.fail(0, 2)
		f.connect(0)
		store.failReport = true
		f.sync()
		if b.Mode() != Local {
			t.Fatalf("Mode() = %v, want local", b.Mode())
		}
		store.failReport = false
		f.sync()
		if b.Mode() != Distributed {
			t.Fatalf("Mode() = %v, want distributed", b.Mode())
		}
	})
}

type lossyStore struct {
	*testStore
	failRestore bool
}

func (s *lossyStore) Restore(ctx context.Context, name string, snapshot Snapshot) (Snapshot, error) {
	if s.failRestore {
		return Snapshot{}, errStoreDown
	}
	return s.testStore.Restore(ctx, name, snapshot)
}

func TestObservability(t *testing.T) {
	t.Run("OB-1 OB-3 the observer gets each transition, mode change, and reconciliation", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.disconnect(0)
		f.connect(0)
		f.sync()
		want := []EventKind{StateChanged, StateChanged, StateChanged, StoreFault, StoreFault, StoreFault, ModeChanged, Reconciled, ModeChanged}
		if got := f.events[0].kinds(); !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		list := f.events[0].list
		if e := list[0]; e.Breaker != sharedName || e.From != Closed || e.To != Open || e.Generation != 1 || e.Mode != Distributed {
			t.Fatalf("first event = %+v", e)
		}
		if e := list[1]; e.From != Open || e.To != HalfOpen || e.Generation != 2 {
			t.Fatalf("second event = %+v", e)
		}
		if e := list[2]; e.From != HalfOpen || e.To != Closed || e.Generation != 3 {
			t.Fatalf("third event = %+v", e)
		}
		if e := list[3]; !errors.Is(e.Err, faultvane.ErrStoreUnavailable) || e.Mode != Distributed {
			t.Fatalf("store fault event = %+v", e)
		}
		if e := list[6]; e.Mode != Local || !errors.Is(e.Err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("mode event = %+v", e)
		}
		if e := list[8]; e.Mode != Distributed || e.Err != nil {
			t.Fatalf("last event = %+v", e)
		}
	})

	t.Run("OB-1 the statistics count calls, rejections, probes, and transitions", func(t *testing.T) {
		b, clock := newTestBreaker(t, WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(1), WithFailureRateThreshold(0.5))
		runTimes(t, b, 2, nil)
		runTimes(t, b, 1, context.Canceled)
		_ = b.Do(context.Background(), func(context.Context) error {
			clock.Advance(time.Second)
			return errDependency
		})
		runTimes(t, b, 1, errDependency)
		assertRejected(t, b, ErrCircuitOpen)
		clock.Advance(testOpenDuration)
		probe, _ := b.Allow(context.Background())
		assertRejected(t, b, ErrProbeNotPermitted)
		probe.Failure()
		clock.Advance(testOpenDuration)
		runTimes(t, b, 1, nil)
		want := Stats{
			State: Closed, Mode: Local,
			Successes: 3, Failures: 3, Ignored: 1, SlowCalls: 1,
			RejectedOpen: 1, RejectedProbe: 1, ProbeSuccesses: 1, ProbeFailures: 1,
			Opened: 2, HalfOpened: 2, Closed: 1,
		}
		if got := b.Stats(); got != want {
			t.Fatalf("Stats() = %+v, want %+v", got, want)
		}
	})

	t.Run("OB-3 OB-4 TK-42 TK-43 the breaker logs transitions and mode changes with their level", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f := newFleet(t, 1, WithLogger(logger), WithMaxLocalFallbackDuration(time.Minute))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.disconnect(0)
		f.clock.Advance(time.Minute)
		f.sync()
		f.connect(0)
		f.sync()
		type line struct {
			Level, Msg, Breaker, From, To, Mode, Cause string
			Generation                                 uint64
		}
		var lines []line
		for _, raw := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			var l line
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, l)
		}
		want := []line{
			{"WARN", "circuit breaker state changed", sharedName, "closed", "open", "distributed", "", 1},
			{"INFO", "circuit breaker state changed", sharedName, "open", "half_open", "distributed", "", 2},
			{"INFO", "circuit breaker state changed", sharedName, "half_open", "closed", "distributed", "", 3},
			{"DEBUG", "circuit breaker store operation failed", sharedName, "closed", "closed", "distributed", errStoreDown.Error(), 0},
			{"DEBUG", "circuit breaker store operation failed", sharedName, "closed", "closed", "distributed", errStoreDown.Error(), 0},
			{"DEBUG", "circuit breaker store operation failed", sharedName, "closed", "closed", "distributed", errStoreDown.Error(), 0},
			{"WARN", "circuit breaker mode changed", sharedName, "closed", "closed", "local", errStoreDown.Error(), 3},
			{"ERROR", "circuit breaker local mode is longer than the maximum time", sharedName, "closed", "closed", "local", "", 3},
			{"INFO", "circuit breaker reconciled with the store", sharedName, "closed", "closed", "distributed", "", 3},
			{"INFO", "circuit breaker mode changed", sharedName, "closed", "closed", "distributed", "", 3},
		}
		if !slices.Equal(lines, want) {
			t.Fatalf("log lines:\n%+v\nwant:\n%+v", lines, want)
		}
	})

	t.Run("OB-3 a panic of the observer is logged and does not reach the caller", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
		b, _ := newTestBreaker(t, WithLogger(logger), WithObserver(panicking{}))
		openBreaker(t, b)
		if !strings.Contains(output.String(), "circuit breaker observer panic") || !strings.Contains(output.String(), "observer failed") {
			t.Fatalf("log = %q", output.String())
		}
	})

	t.Run("OB-1 the names of modes and event kinds", func(t *testing.T) {
		names := []string{Distributed.String(), Local.String(), StateChanged.String(), ModeChanged.String(), Reconciled.String(), StoreFault.String(), LocalModeExpired.String(), ConfigMismatchChanged.String(), EventKind(99).String()}
		want := []string{"distributed", "local", "state_changed", "mode_changed", "reconciled", "store_fault", "local_mode_expired", "config_mismatch_changed", "unknown"}
		if !slices.Equal(names, want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
	})

	t.Run("API-1 TK-30 a breaker without a store is in local mode", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		if b.Mode() != Local {
			t.Fatalf("Mode() = %v, want local", b.Mode())
		}
	})
}

type panicking struct{}

func (panicking) Observe(Event) { panic("observer failed") }
