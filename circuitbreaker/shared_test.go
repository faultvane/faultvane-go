package circuitbreaker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/faultvanetest"
)

const sharedName = "payment-service"

var errStoreDown = fmt.Errorf("connection refused: %w", faultvane.ErrStoreUnavailable)

type testStore struct {
	Store
	down       atomic.Bool
	hang       atomic.Bool
	operations atomic.Int64
	acquires   atomic.Int64
	restores   []Snapshot
	mu         sync.Mutex
}

func (s *testStore) before(ctx context.Context) error {
	s.operations.Add(1)
	if s.hang.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.down.Load() {
		return errStoreDown
	}
	return nil
}

func (s *testStore) Load(ctx context.Context, name string) (Snapshot, error) {
	if err := s.before(ctx); err != nil {
		return Snapshot{}, err
	}
	return s.Store.Load(ctx, name)
}

func (s *testStore) Report(ctx context.Context, name string, report Report) (Snapshot, error) {
	if err := s.before(ctx); err != nil {
		return Snapshot{}, err
	}
	return s.Store.Report(ctx, name, report)
}

func (s *testStore) AcquireProbe(ctx context.Context, name string, request ProbeRequest) (ProbeGrant, error) {
	s.acquires.Add(1)
	if err := s.before(ctx); err != nil {
		return ProbeGrant{}, err
	}
	return s.Store.AcquireProbe(ctx, name, request)
}

func (s *testStore) CompleteProbe(ctx context.Context, name string, result ProbeResult) (Snapshot, error) {
	if err := s.before(ctx); err != nil {
		return Snapshot{}, err
	}
	return s.Store.CompleteProbe(ctx, name, result)
}

func (s *testStore) Restore(ctx context.Context, name string, snapshot Snapshot) (Snapshot, error) {
	if err := s.before(ctx); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	s.restores = append(s.restores, snapshot)
	s.mu.Unlock()
	return s.Store.Restore(ctx, name, snapshot)
}

type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) Observe(event Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, event)
}

func (e *events) kinds() []EventKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	kinds := make([]EventKind, len(e.list))
	for i, event := range e.list {
		kinds[i] = event.Kind
	}
	return kinds
}

func (e *events) count(kind EventKind) int {
	total := 0
	for _, k := range e.kinds() {
		if k == kind {
			total++
		}
	}
	return total
}

type fleet struct {
	t        *testing.T
	clock    *faultvanetest.Clock
	memory   *MemoryStore
	stores   []*testStore
	breakers []*Breaker
	events   []*events
	options  []Option
}

func newFleet(t *testing.T, instances int, opts ...Option) *fleet {
	t.Helper()
	clock := faultvanetest.NewClock(testStart)
	f := &fleet{t: t, clock: clock, memory: NewMemoryStore(clock), options: opts}
	for range instances {
		f.add()
	}
	return f
}

func (f *fleet) add(extra ...Option) *Breaker {
	f.t.Helper()
	store := &testStore{Store: f.memory}
	return f.addWithStore(store, extra...)
}

func (f *fleet) addWithStore(store *testStore, extra ...Option) *Breaker {
	f.t.Helper()
	observer := &events{}
	base := []Option{
		WithClock(f.clock), WithLogger(quiet), WithStore(store), WithObserver(observer),
		WithFailureThreshold(5), WithMinimumCalls(1000), WithOpenDuration(testOpenDuration),
		WithProbeTimeout(testLease), WithProbeAttemptInterval(0), WithStoreTimeout(50 * time.Millisecond),
		func(c *config) { c.manualSync = true },
	}
	b, err := New(sharedName, append(append(base, f.options...), extra...)...)
	if err != nil {
		f.t.Fatal(err)
	}
	f.stores = append(f.stores, store)
	f.breakers = append(f.breakers, b)
	f.events = append(f.events, observer)
	return b
}

func (f *fleet) sync(indexes ...int) {
	if len(indexes) == 0 {
		for i := range f.breakers {
			indexes = append(indexes, i)
		}
	}
	for _, i := range indexes {
		f.breakers[i].synchronize(context.Background())
	}
}

func (f *fleet) fail(index, times int) {
	f.t.Helper()
	runTimes(f.t, f.breakers[index], times, errDependency)
}

func (f *fleet) stored() Record {
	f.memory.mu.Lock()
	defer f.memory.mu.Unlock()
	return f.memory.records[sharedName]
}

func (f *fleet) assertStored(state State, generation uint64) {
	f.t.Helper()
	if r := f.stored(); r.State != state || r.Generation != generation {
		f.t.Fatalf("store has %v generation %d, want %v generation %d", r.State, r.Generation, state, generation)
	}
}

func (f *fleet) assertAll(state State, mode Mode) {
	f.t.Helper()
	for i, b := range f.breakers {
		if b.State() != state || b.Mode() != mode {
			f.t.Fatalf("instance %d: %v in %v mode, want %v in %v mode", i, b.State(), b.Mode(), state, mode)
		}
	}
}

func (f *fleet) openAll() {
	f.t.Helper()
	f.fail(0, 5)
	f.sync()
	f.assertAll(Open, Distributed)
	f.assertStored(Open, 1)
}

func (f *fleet) disconnect(indexes ...int) {
	if len(indexes) == 0 {
		for i := range f.breakers {
			indexes = append(indexes, i)
		}
	}
	for _, i := range indexes {
		f.stores[i].down.Store(true)
	}
	for range 3 {
		f.sync(indexes...)
	}
}

func (f *fleet) connect(indexes ...int) {
	for _, i := range indexes {
		f.stores[i].down.Store(false)
	}
}

func TestSharedState(t *testing.T) {
	t.Run("DS-1 DS-3 RC-6 a new instance starts from the state in the store", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		late := f.add()
		if late.State() != Open || late.Mode() != Distributed {
			t.Fatalf("new instance: %v in %v mode, want open in distributed mode", late.State(), late.Mode())
		}
		assertRejected(t, late, ErrCircuitOpen)
	})

	t.Run("DS-2 a new instance keeps the rest of the open duration", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(10 * time.Second)
		late := f.add()
		f.clock.Advance(testOpenDuration - 10*time.Second - time.Nanosecond)
		assertRejected(t, late, ErrCircuitOpen)
		if got := f.stores[1].acquires.Load(); got != 0 {
			t.Fatalf("probe requests = %d before the open duration ended, want 0", got)
		}
		f.clock.Advance(time.Nanosecond)
		runTimes(t, late, 1, nil)
		f.assertStored(Closed, 3)
	})

	t.Run("DS-1 an instance observes the transition of a different instance at its next synchronization", func(t *testing.T) {
		f := newFleet(t, 2)
		f.fail(0, 5)
		f.sync(0)
		if f.breakers[0].State() != Open || f.breakers[1].State() != Closed {
			t.Fatalf("states = %v, %v, want open, closed", f.breakers[0].State(), f.breakers[1].State())
		}
		f.sync(1)
		f.assertAll(Open, Distributed)
	})

	t.Run("FD-6 AT-1 ten instances with one failure each open a circuit with a threshold of five", func(t *testing.T) {
		f := newFleet(t, 10)
		target := &dependency{}
		for _, b := range f.breakers {
			_ = b.Do(context.Background(), target.respond(errDependency))
		}
		f.assertAll(Closed, Distributed)
		f.sync()
		f.sync()
		f.assertAll(Open, Distributed)
		f.assertStored(Open, 1)
		for _, b := range f.breakers {
			if s := b.Stats(); s.Failures != 1 {
				t.Fatalf("an instance has %d failures, want 1", s.Failures)
			}
			_ = b.Do(context.Background(), target.respond(nil))
		}
		if target.calls.Load() != 10 {
			t.Fatalf("the dependency got %d calls, want 10", target.calls.Load())
		}
	})

	t.Run("FD-7 PF-1 PF-3 TK-35 calls make no store operation and one synchronization sends one report", func(t *testing.T) {
		f := newFleet(t, 1)
		before := f.stores[0].operations.Load()
		runTimes(t, f.breakers[0], 1000, nil)
		runTimes(t, f.breakers[0], 4, errDependency)
		if got := f.stores[0].operations.Load(); got != before {
			t.Fatalf("store operations went from %d to %d during calls", before, got)
		}
		f.sync()
		if got := f.stores[0].operations.Load(); got != before+1 {
			t.Fatalf("store operations = %d, want %d", got, before+1)
		}
		bucket := f.stored().Buckets[0]
		if bucket.Calls != 1004 || bucket.Failures != 4 {
			t.Fatalf("shared window has %d calls and %d failures, want 1004 and 4", bucket.Calls, bucket.Failures)
		}
	})

	t.Run("FD-4 slow calls reach the shared window", func(t *testing.T) {
		f := newFleet(t, 1, WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(1))
		_ = f.breakers[0].Do(context.Background(), func(context.Context) error {
			f.clock.Advance(time.Second)
			return nil
		})
		f.sync()
		if bucket := f.stored().Buckets[0]; bucket.SlowCalls != 1 || f.breakers[0].Stats().SlowCalls != 1 {
			t.Fatalf("shared window has %d slow calls, want 1", bucket.SlowCalls)
		}
	})

	t.Run("DS-2 a synchronization without calls reads the state", func(t *testing.T) {
		f := newFleet(t, 2)
		f.fail(0, 5)
		f.sync(0)
		f.sync(1)
		if f.breakers[1].State() != Open || len(f.stored().Buckets) != 0 {
			t.Fatalf("state = %v, buckets = %d", f.breakers[1].State(), len(f.stored().Buckets))
		}
	})

	t.Run("GEN-3 AT-5 the store ignores a report with an old generation", func(t *testing.T) {
		f := newFleet(t, 2)
		f.fail(0, 4)
		f.fail(1, 5)
		f.sync(1)
		f.assertStored(Open, 1)
		f.sync(0)
		f.assertStored(Open, 1)
		if f.breakers[0].State() != Open || f.breakers[0].current.Load().generation != 1 {
			t.Fatalf("stale instance: %v generation %d, want open generation 1", f.breakers[0].State(), f.breakers[0].current.Load().generation)
		}
	})

	t.Run("GEN-3 SM-7 late failures of an older generation do not reach the new closed period", func(t *testing.T) {
		f := newFleet(t, 2)
		f.fail(0, 4)
		f.openAllFrom(1)
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[1], 1, nil)
		f.assertStored(Closed, 3)
		f.sync(0)
		if stored := f.stored(); stored.State != Closed || stored.Generation != 3 || len(stored.Buckets) != 0 {
			t.Fatalf("store = %+v, want closed generation 3 with an empty window", stored)
		}
		f.assertAll(Closed, Distributed)
	})

	t.Run("GEN-3 a result of a call from before a transition does not count for the new generation", func(t *testing.T) {
		f := newFleet(t, 1)
		late, _ := f.breakers[0].Allow(context.Background())
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(Closed, 3)
		late.Failure()
		f.sync()
		if len(f.stored().Buckets) != 0 {
			t.Fatalf("the shared window has %d buckets, want 0", len(f.stored().Buckets))
		}
	})

	t.Run("FD-7 a failed report is not sent again", func(t *testing.T) {
		f := newFleet(t, 1)
		f.fail(0, 4)
		f.stores[0].down.Store(true)
		f.sync()
		f.stores[0].down.Store(false)
		f.sync()
		if stored := f.stored(); len(stored.Buckets) != 0 || stored.State != Closed {
			t.Fatalf("store = %+v, want closed with an empty window", stored)
		}
	})
}

func (f *fleet) openAllFrom(index int) {
	f.t.Helper()
	f.fail(index, 5)
	f.sync(index)
	f.assertStored(Open, 1)
}

func TestSharedProbe(t *testing.T) {
	t.Run("HO-1 HO-2 HO-4 AT-2 exactly one of ten instances gets the probe", func(t *testing.T) {
		for _, result := range []error{nil, errDependency} {
			f := newFleet(t, 10)
			f.openAll()
			f.clock.Advance(testOpenDuration)
			var permits []Permit
			var group sync.WaitGroup
			var mu sync.Mutex
			var notPermitted atomic.Int64
			for _, b := range f.breakers {
				group.Add(1)
				go func() {
					defer group.Done()
					permit, err := b.Allow(context.Background())
					if err == ErrProbeNotPermitted {
						notPermitted.Add(1)
					}
					if err == nil {
						mu.Lock()
						permits = append(permits, permit)
						mu.Unlock()
					}
				}()
			}
			group.Wait()
			if len(permits) != 1 || notPermitted.Load() != 9 {
				t.Fatalf("permits = %d, not permitted = %d, want 1 and 9", len(permits), notPermitted.Load())
			}
			f.assertStored(HalfOpen, 2)
			permits[0].complete(DefaultClassifier(result))
			f.sync()
			if result == nil {
				f.assertAll(Closed, Distributed)
				f.assertStored(Closed, 3)
			} else {
				f.assertAll(Open, Distributed)
				f.assertStored(Open, 3)
			}
		}
	})

	t.Run("HO-5 PF-1 concurrent calls of one instance make one probe request", func(t *testing.T) {
		f := newFleet(t, 1, WithProbeAttemptInterval(100*time.Millisecond))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		var group sync.WaitGroup
		var granted atomic.Int64
		for range 64 {
			group.Add(1)
			go func() {
				defer group.Done()
				if _, err := f.breakers[0].Allow(context.Background()); err == nil {
					granted.Add(1)
				}
			}()
		}
		group.Wait()
		if granted.Load() != 1 || f.stores[0].acquires.Load() != 1 {
			t.Fatalf("granted = %d, probe requests = %d, want 1 and 1", granted.Load(), f.stores[0].acquires.Load())
		}
	})

	t.Run("HO-5 an instance sends the next probe request after the attempt interval", func(t *testing.T) {
		f := newFleet(t, 2, WithProbeAttemptInterval(100*time.Millisecond))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		if _, err := f.breakers[0].Allow(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, f.breakers[1], ErrProbeNotPermitted)
		f.clock.Advance(100*time.Millisecond - time.Nanosecond)
		assertRejected(t, f.breakers[1], ErrProbeNotPermitted)
		if got := f.stores[1].acquires.Load(); got != 1 {
			t.Fatalf("probe requests = %d, want 1", got)
		}
		f.clock.Advance(time.Nanosecond)
		assertRejected(t, f.breakers[1], ErrProbeNotPermitted)
		if got := f.stores[1].acquires.Load(); got != 2 {
			t.Fatalf("probe requests = %d, want 2", got)
		}
	})

	t.Run("HO-3 the lease of an instance that stopped ends and a different instance gets it", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		lost, err := f.breakers[0].Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(testLease - time.Nanosecond)
		assertRejected(t, f.breakers[1], ErrProbeNotPermitted)
		f.clock.Advance(time.Nanosecond)
		current, err := f.breakers[1].Allow(context.Background())
		if err != nil {
			t.Fatalf("Allow() error = %v, want the lease", err)
		}
		lost.Failure()
		f.assertStored(HalfOpen, 2)
		current.Success()
		f.assertStored(Closed, 3)
	})

	t.Run("HO-2 a probe request finds a circuit that a different instance closed", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(Closed, 3)
		target := &dependency{}
		if err := f.breakers[1].Do(context.Background(), target.respond(nil)); err != nil || target.calls.Load() != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, target.calls.Load())
		}
		if f.breakers[1].State() != Closed {
			t.Fatalf("State() = %v, want closed", f.breakers[1].State())
		}
		f.sync(1)
		if bucket := f.stored().Buckets[0]; bucket.Calls != 1 {
			t.Fatalf("shared window has %d calls, want 1", bucket.Calls)
		}
	})

	t.Run("HO-2 a probe request finds a circuit that a different instance opened again", func(t *testing.T) {
		f := newFleet(t, 2)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, errDependency)
		f.assertStored(Open, 3)
		assertRejected(t, f.breakers[1], ErrCircuitOpen)
		if f.breakers[1].State() != Open {
			t.Fatalf("State() = %v, want open", f.breakers[1].State())
		}
	})

	t.Run("RF-2 ERR-3 a probe request that the store does not answer is a rejection", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		f.stores[0].down.Store(true)
		assertRejected(t, f.breakers[0], ErrProbeNotPermitted)
		if s := f.breakers[0].Stats(); s.StoreErrors != 1 || s.RejectedProbe != 1 {
			t.Fatalf("stats = %+v", s)
		}
	})

	t.Run("RF-2 DG-3 a probe request waits for the store only for the store timeout", func(t *testing.T) {
		f := newFleet(t, 1, WithStoreTimeout(10*time.Millisecond))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		f.stores[0].hang.Store(true)
		assertRejected(t, f.breakers[0], ErrProbeNotPermitted)
	})

	t.Run("RF-3 a probe result that the store does not accept is not sent again", func(t *testing.T) {
		f := newFleet(t, 1)
		f.openAll()
		f.clock.Advance(testOpenDuration)
		probe, err := f.breakers[0].Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		f.stores[0].down.Store(true)
		before := f.stores[0].operations.Load()
		probe.Success()
		if got := f.stores[0].operations.Load(); got != before+1 {
			t.Fatalf("store operations = %d, want %d", got, before+1)
		}
		f.stores[0].down.Store(false)
		f.assertStored(HalfOpen, 2)
		f.clock.Advance(testLease)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(Closed, 3)
	})

	t.Run("SM-5 HO-1 the probe limit of the configuration applies to all instances together", func(t *testing.T) {
		f := newFleet(t, 3, WithHalfOpenMaxProbes(2), WithHalfOpenSuccessThreshold(2))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		first, firstErr := f.breakers[0].Allow(context.Background())
		second, secondErr := f.breakers[1].Allow(context.Background())
		if firstErr != nil || secondErr != nil {
			t.Fatalf("Allow() errors = %v, %v, want two probes", firstErr, secondErr)
		}
		assertRejected(t, f.breakers[2], ErrProbeNotPermitted)
		first.Success()
		second.Success()
		f.assertStored(Closed, 3)
	})

	t.Run("SM-6 the success threshold applies to the probes of all instances", func(t *testing.T) {
		f := newFleet(t, 2, WithHalfOpenSuccessThreshold(2))
		f.openAll()
		f.clock.Advance(testOpenDuration)
		runTimes(t, f.breakers[0], 1, nil)
		f.assertStored(HalfOpen, 2)
		runTimes(t, f.breakers[1], 1, nil)
		f.assertStored(Closed, 3)
	})
}

func (f *fleet) openAllLocal() {
	f.t.Helper()
	for i, b := range f.breakers {
		for range 1000 {
			_ = run(b, errDependency)
		}
		if b.State() != Open || b.Mode() != Local {
			f.t.Fatalf("instance %d: %v in %v mode, want open in local mode", i, b.State(), b.Mode())
		}
	}
}
