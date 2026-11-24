package circuitbreaker

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/faultvanetest"
)

var errDependency = errors.New("dependency failed")

var quiet = slog.New(slog.DiscardHandler)

type dependency struct {
	calls atomic.Int64
}

func (d *dependency) respond(err error) func(context.Context) error {
	return func(context.Context) error {
		d.calls.Add(1)
		return err
	}
}

func newTestBreaker(t *testing.T, opts ...Option) (*Breaker, *faultvanetest.Clock) {
	t.Helper()
	clock := faultvanetest.NewClock(testStart)
	base := []Option{WithClock(clock), WithLogger(quiet), WithMinimumCalls(4), WithOpenDuration(testOpenDuration), WithProbeTimeout(testLease)}
	b, err := New("payment-service", append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return b, clock
}

func run(b *Breaker, err error) error {
	return b.Do(context.Background(), func(context.Context) error { return err })
}

func runTimes(t *testing.T, b *Breaker, times int, result error) {
	t.Helper()
	for range times {
		if err := run(b, result); err != result {
			t.Fatalf("Do() = %v, want %v", err, result)
		}
	}
}

func openBreaker(t *testing.T, b *Breaker) {
	t.Helper()
	runTimes(t, b, 4, errDependency)
	assertState(t, b, Open)
}

func halfOpenBreaker(t *testing.T, b *Breaker, clock *faultvanetest.Clock) Permit {
	t.Helper()
	openBreaker(t, b)
	clock.Advance(testOpenDuration)
	permit, err := b.Allow(context.Background())
	if err != nil {
		t.Fatalf("Allow() error = %v, want a probe permit", err)
	}
	assertState(t, b, HalfOpen)
	return permit
}

func assertState(t *testing.T, b *Breaker, want State) {
	t.Helper()
	if got := b.State(); got != want {
		t.Fatalf("State() = %v, want %v", got, want)
	}
}

func assertRejected(t *testing.T, b *Breaker, want error) {
	t.Helper()
	target := &dependency{}
	err := b.Do(context.Background(), target.respond(nil))
	if err != want || target.calls.Load() != 0 {
		t.Fatalf("Do() = %v with %d calls of the dependency, want %v with 0 calls", err, target.calls.Load(), want)
	}
}

func generation(b *Breaker) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.record.Generation
}

func TestClosed(t *testing.T) {
	t.Run("SM-1 a new breaker is closed and has its name", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		assertState(t, b, Closed)
		if b.Name() != "payment-service" {
			t.Fatalf("Name() = %q", b.Name())
		}
	})

	t.Run("SM-2 API-1 a closed breaker runs the call and returns its result", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		target := &dependency{}
		if err := b.Do(context.Background(), target.respond(nil)); err != nil || target.calls.Load() != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, target.calls.Load())
		}
	})

	t.Run("SM-2 FD-2 the breaker opens when the failure rate reaches the threshold", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 2, nil)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Closed)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("FD-2 the breaker stays closed below the minimum number of calls", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 3, errDependency)
		assertState(t, b, Closed)
	})

	t.Run("FD-2 a success that completes the minimum number of calls opens the breaker", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 3, errDependency)
		runTimes(t, b, 1, nil)
		assertState(t, b, Open)
	})

	t.Run("FD-2 the breaker stays closed below the failure rate", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 3, nil)
		runTimes(t, b, 1, errDependency)
		runTimes(t, b, 20, nil)
		assertState(t, b, Closed)
	})

	t.Run("FD-1 the breaker opens at the failure count threshold", func(t *testing.T) {
		b, _ := newTestBreaker(t, WithMinimumCalls(100), WithFailureThreshold(3))
		runTimes(t, b, 50, nil)
		runTimes(t, b, 2, errDependency)
		assertState(t, b, Closed)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("FD-3 failures that left the window do not count", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		runTimes(t, b, 3, errDependency)
		clock.Advance(10 * time.Second)
		runTimes(t, b, 3, errDependency)
		assertState(t, b, Closed)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("FD-3 failures in different buckets of the window count together", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		for range 3 {
			runTimes(t, b, 1, errDependency)
			clock.Advance(3 * time.Second)
		}
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})
}

func TestSlowCalls(t *testing.T) {
	slowOptions := []Option{WithSlowCallDuration(2 * time.Second), WithSlowCallRateThreshold(0.5), WithFailureRateThreshold(1)}
	take := func(b *Breaker, clock *faultvanetest.Clock, d time.Duration) {
		_ = b.Do(context.Background(), func(context.Context) error {
			clock.Advance(d)
			return nil
		})
	}

	t.Run("FD-4 the breaker opens when the slow call rate reaches the threshold", func(t *testing.T) {
		b, clock := newTestBreaker(t, slowOptions...)
		runTimes(t, b, 2, nil)
		take(b, clock, 2*time.Second)
		assertState(t, b, Closed)
		take(b, clock, 2*time.Second)
		assertState(t, b, Open)
	})

	t.Run("FD-4 a call shorter than the slow call duration is not slow", func(t *testing.T) {
		b, clock := newTestBreaker(t, slowOptions...)
		for range 4 {
			take(b, clock, 2*time.Second-time.Nanosecond)
		}
		assertState(t, b, Closed)
	})

	t.Run("FD-4 slow calls do not count when slow call detection is off", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		for range 4 {
			take(b, clock, time.Second)
		}
		assertState(t, b, Closed)
	})
}

func TestOpen(t *testing.T) {
	t.Run("SM-3 ERR-1 an open breaker rejects calls and does not call the dependency", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		openBreaker(t, b)
		assertRejected(t, b, ErrCircuitOpen)
	})

	t.Run("SM-3 an open breaker stays open before the open duration ends", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		clock.Advance(testOpenDuration - time.Nanosecond)
		assertRejected(t, b, ErrCircuitOpen)
		assertState(t, b, Open)
	})

	t.Run("SM-4 the first call after the open duration is a probe", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		clock.Advance(testOpenDuration)
		assertState(t, b, Open)
		target := &dependency{}
		entered := State(99)
		err := b.Do(context.Background(), func(ctx context.Context) error {
			entered = b.State()
			return target.respond(nil)(ctx)
		})
		if err != nil || target.calls.Load() != 1 || entered != HalfOpen {
			t.Fatalf("Do() = %v, calls = %d, state in the call = %v", err, target.calls.Load(), entered)
		}
	})

	t.Run("SM-2 the open duration starts at the transition", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		runTimes(t, b, 3, errDependency)
		clock.Advance(5 * time.Second)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
		clock.Advance(testOpenDuration - time.Nanosecond)
		assertRejected(t, b, ErrCircuitOpen)
		clock.Advance(time.Nanosecond)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})
}

func TestHalfOpen(t *testing.T) {
	t.Run("SM-5 HO-5 ERR-1 only one probe runs with the default limit", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		halfOpenBreaker(t, b, clock)
		assertRejected(t, b, ErrProbeNotPermitted)
		assertRejected(t, b, ErrProbeNotPermitted)
		assertState(t, b, HalfOpen)
	})

	t.Run("SM-5 the probe limit is configurable", func(t *testing.T) {
		b, clock := newTestBreaker(t, WithHalfOpenMaxProbes(2))
		halfOpenBreaker(t, b, clock)
		if _, err := b.Allow(context.Background()); err != nil {
			t.Fatalf("second Allow() error = %v, want nil", err)
		}
		assertRejected(t, b, ErrProbeNotPermitted)
	})

	t.Run("SM-6 a successful probe closes the breaker", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		halfOpenBreaker(t, b, clock).Success()
		assertState(t, b, Closed)
		runTimes(t, b, 1, nil)
	})

	t.Run("SM-6 a failed probe opens the breaker for a new open duration", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		probe := halfOpenBreaker(t, b, clock)
		clock.Advance(time.Second)
		probe.Failure()
		assertState(t, b, Open)
		clock.Advance(testOpenDuration - time.Nanosecond)
		assertRejected(t, b, ErrCircuitOpen)
		clock.Advance(time.Nanosecond)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})

	t.Run("SM-6 the breaker closes only at the success threshold", func(t *testing.T) {
		b, clock := newTestBreaker(t, WithHalfOpenSuccessThreshold(2))
		halfOpenBreaker(t, b, clock).Success()
		assertState(t, b, HalfOpen)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})

	t.Run("SM-6 an ignored probe gives its place to the next call", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		halfOpenBreaker(t, b, clock).Ignore()
		assertState(t, b, HalfOpen)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})

	t.Run("HO-3 a probe without a result gives its place after the probe timeout", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		lost := halfOpenBreaker(t, b, clock)
		clock.Advance(testLease - time.Nanosecond)
		assertRejected(t, b, ErrProbeNotPermitted)
		clock.Advance(time.Nanosecond)
		lost.Success()
		assertState(t, b, HalfOpen)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})

	t.Run("SM-2 a closed breaker after recovery has an empty window", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		halfOpenBreaker(t, b, clock).Success()
		runTimes(t, b, 3, errDependency)
		assertState(t, b, Closed)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("SM-7 a probe request on a breaker that closed in the meantime gets a normal permit", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		permit, err := b.acquireLocalProbe(clock.Now())
		if err != nil || permit.token != 0 || permit.view != b.current.Load() {
			t.Fatalf("acquireLocalProbe() = %+v, %v, want a normal permit", permit, err)
		}
	})

	t.Run("SM-7 a probe request on a breaker that opened again in the meantime is rejected", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		if _, err := b.acquireLocalProbe(clock.Now()); err != ErrCircuitOpen {
			t.Fatalf("acquireLocalProbe() error = %v, want ErrCircuitOpen", err)
		}
		assertState(t, b, Open)
	})
}

func TestGenerations(t *testing.T) {
	t.Run("GEN-1 each transition increases the generation by 1", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		if got := generation(b); got != 0 {
			t.Fatalf("generation = %d, want 0", got)
		}
		openBreaker(t, b)
		if got := generation(b); got != 1 {
			t.Fatalf("generation = %d, want 1", got)
		}
		clock.Advance(testOpenDuration)
		probe, _ := b.Allow(context.Background())
		if got := generation(b); got != 2 {
			t.Fatalf("generation = %d, want 2", got)
		}
		probe.Success()
		if got := generation(b); got != 3 {
			t.Fatalf("generation = %d, want 3", got)
		}
	})

	t.Run("SM-7 GEN-3 a late success of a call from before the transition does not close an open breaker", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		late, _ := b.Allow(context.Background())
		openBreaker(t, b)
		late.Success()
		assertState(t, b, Open)
		if got := generation(b); got != 1 {
			t.Fatalf("generation = %d, want 1", got)
		}
	})

	t.Run("SM-7 GEN-3 late failures from an older closed period do not open the new closed period", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		var late []Permit
		for range 10 {
			permit, _ := b.Allow(context.Background())
			late = append(late, permit)
		}
		halfOpenBreaker(t, b, clock).Success()
		assertState(t, b, Closed)
		for _, permit := range late {
			permit.Failure()
		}
		assertState(t, b, Closed)
		runTimes(t, b, 3, errDependency)
		assertState(t, b, Closed)
	})

	t.Run("SM-7 a late result of a probe from an older half-open period has no effect", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		old := halfOpenBreaker(t, b, clock)
		old.Failure()
		assertState(t, b, Open)
		clock.Advance(testOpenDuration)
		current, err := b.Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		old.Success()
		assertState(t, b, HalfOpen)
		assertRejected(t, b, ErrProbeNotPermitted)
		current.Success()
		assertState(t, b, Closed)
	})
}

func TestSnapshot(t *testing.T) {
	t.Run("GEN-3 an event that changes no state keeps the snapshot and its window", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		runTimes(t, b, 3, errDependency)
		before := b.current.Load()
		b.trip(99, clock.Now())
		b.completeLocalProbe(0, 1, Failure, clock.Now())
		if b.current.Load() != before {
			t.Fatal("the snapshot changed")
		}
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("SM-5 a rejected probe request keeps the snapshot", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		halfOpenBreaker(t, b, clock)
		before := b.current.Load()
		assertRejected(t, b, ErrProbeNotPermitted)
		if b.current.Load() != before {
			t.Fatal("the snapshot changed")
		}
	})
}

func TestErrors(t *testing.T) {
	t.Run("ERR-2 TK-11 each rejection matches ErrRejected and itself only", func(t *testing.T) {
		if !errors.Is(ErrCircuitOpen, faultvane.ErrRejected) || !errors.Is(ErrProbeNotPermitted, faultvane.ErrRejected) {
			t.Fatal("a rejection does not match ErrRejected")
		}
		if errors.Is(ErrCircuitOpen, ErrProbeNotPermitted) || errors.Is(ErrProbeNotPermitted, ErrCircuitOpen) {
			t.Fatal("the two rejections match each other")
		}
		if ErrCircuitOpen.Error() != "circuitbreaker: circuit is open: faultvane: call rejected" || ErrProbeNotPermitted.Error() != "circuitbreaker: probe is not permitted: faultvane: call rejected" {
			t.Fatalf("messages = %q, %q", ErrCircuitOpen, ErrProbeNotPermitted)
		}
	})

	t.Run("ERR-2 TK-12 the error of the dependency returns without a change and is not a rejection", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		err := run(b, errDependency)
		if err != errDependency || errors.Is(err, faultvane.ErrRejected) {
			t.Fatalf("Do() = %v", err)
		}
	})
}

func TestClassifier(t *testing.T) {
	t.Run("FD-5 the default classifier", func(t *testing.T) {
		cases := []struct {
			err  error
			want Result
		}{
			{nil, Success},
			{errDependency, Failure},
			{context.DeadlineExceeded, Failure},
			{context.Canceled, Ignored},
			{errors.Join(errDependency, context.Canceled), Ignored},
		}
		for _, c := range cases {
			if got := DefaultClassifier(c.err); got != c.want {
				t.Errorf("DefaultClassifier(%v) = %v, want %v", c.err, got, c.want)
			}
		}
	})

	t.Run("FD-5 a call that the caller cancelled does not count", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 10, context.Canceled)
		runTimes(t, b, 3, errDependency)
		assertState(t, b, Closed)
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
	})

	t.Run("FD-5 a custom classifier decides the result", func(t *testing.T) {
		errNotFound := errors.New("not found")
		b, _ := newTestBreaker(t, WithClassifier(func(err error) Result {
			if errors.Is(err, errNotFound) {
				return Success
			}
			return DefaultClassifier(err)
		}))
		runTimes(t, b, 10, errNotFound)
		assertState(t, b, Closed)
		runTimes(t, b, 10, errDependency)
		assertState(t, b, Open)
	})

	t.Run("FD-5 the classifier decides the result of a probe", func(t *testing.T) {
		b, clock := newTestBreaker(t, WithClassifier(func(error) Result { return Failure }))
		openBreaker(t, b)
		clock.Advance(testOpenDuration)
		runTimes(t, b, 1, nil)
		assertState(t, b, Open)
	})
}

func TestContext(t *testing.T) {
	t.Run("API-3 TK-13 a cancelled context returns its error and runs nothing", func(t *testing.T) {
		b, _ := newTestBreaker(t, WithMinimumCalls(1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		target := &dependency{}
		for range 5 {
			if err := b.Do(ctx, target.respond(errDependency)); err != context.Canceled {
				t.Fatalf("Do() = %v, want context.Canceled", err)
			}
		}
		if _, err := b.Allow(ctx); err != context.Canceled {
			t.Fatalf("Allow() error = %v, want context.Canceled", err)
		}
		if target.calls.Load() != 0 {
			t.Fatalf("calls = %d, want 0", target.calls.Load())
		}
		assertState(t, b, Closed)
	})

	t.Run("API-3 a cancelled context does not use a probe", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		clock.Advance(testOpenDuration)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runWithContext(ctx, b); err != context.Canceled {
			t.Fatalf("Do() = %v, want context.Canceled", err)
		}
		assertState(t, b, Open)
		runTimes(t, b, 1, nil)
		assertState(t, b, Closed)
	})

	t.Run("API-3 the call gets the context of the caller", func(t *testing.T) {
		type key struct{}
		b, _ := newTestBreaker(t)
		ctx := context.WithValue(context.Background(), key{}, "value")
		var got any
		_ = b.Do(ctx, func(ctx context.Context) error {
			got = ctx.Value(key{})
			return nil
		})
		if got != "value" {
			t.Fatalf("value = %v", got)
		}
	})
}

func runWithContext(ctx context.Context, b *Breaker) error {
	return b.Do(ctx, func(context.Context) error { return nil })
}

func TestPanic(t *testing.T) {
	panicking := func(b *Breaker) (recovered any) {
		defer func() { recovered = recover() }()
		_ = b.Do(context.Background(), func(context.Context) error { panic("broken") })
		return nil
	}

	t.Run("TK-17 a panic counts as a failure and continues with the same value", func(t *testing.T) {
		b, _ := newTestBreaker(t, WithMinimumCalls(2))
		runTimes(t, b, 1, nil)
		if got := panicking(b); got != "broken" {
			t.Fatalf("recovered = %v, want broken", got)
		}
		assertState(t, b, Open)
	})

	t.Run("TK-17 a panic of a probe opens the breaker", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		clock.Advance(testOpenDuration)
		if got := panicking(b); got != "broken" {
			t.Fatalf("recovered = %v, want broken", got)
		}
		assertState(t, b, Open)
		if got := generation(b); got != 3 {
			t.Fatalf("generation = %d, want 3", got)
		}
	})

	t.Run("TK-17 a call without a panic is recorded one time", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		runTimes(t, b, 4, nil)
		calls, failures, _ := b.current.Load().window.totals(0)
		if calls != 4 || failures != 0 {
			t.Fatalf("calls = %d, failures = %d, want 4, 0", calls, failures)
		}
	})
}

func TestPermit(t *testing.T) {
	t.Run("API-1 the three results of a permit", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		for range 2 {
			permit, err := b.Allow(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			permit.Success()
		}
		for range 5 {
			permit, _ := b.Allow(context.Background())
			permit.Ignore()
		}
		assertState(t, b, Closed)
		for range 2 {
			permit, _ := b.Allow(context.Background())
			permit.Failure()
		}
		assertState(t, b, Open)
	})

	t.Run("API-1 a permit of a rejected call does nothing", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		openBreaker(t, b)
		permit, err := b.Allow(context.Background())
		if err != ErrCircuitOpen {
			t.Fatalf("Allow() error = %v", err)
		}
		permit.Success()
		permit.Failure()
		permit.Ignore()
		assertState(t, b, Open)
	})

	t.Run("FD-4 the permit measures the time from Allow to the result", func(t *testing.T) {
		b, clock := newTestBreaker(t, WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(1), WithFailureRateThreshold(1), WithMinimumCalls(1))
		permit, _ := b.Allow(context.Background())
		clock.Advance(time.Second)
		permit.Success()
		assertState(t, b, Open)
	})
}

func TestExecute(t *testing.T) {
	t.Run("API-1 Execute returns the value of the call", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		value, err := Execute(context.Background(), b, func(context.Context) (string, error) { return "receipt", nil })
		if value != "receipt" || err != nil {
			t.Fatalf("Execute() = %q, %v", value, err)
		}
	})

	t.Run("TK-12 Execute returns the value and the error of the call", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		value, err := Execute(context.Background(), b, func(context.Context) (int, error) { return 7, errDependency })
		if value != 7 || err != errDependency {
			t.Fatalf("Execute() = %d, %v", value, err)
		}
	})

	t.Run("ERR-2 Execute returns the zero value for a rejection", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		openBreaker(t, b)
		value, err := Execute(context.Background(), b, func(context.Context) (int, error) { return 7, nil })
		if value != 0 || err != ErrCircuitOpen {
			t.Fatalf("Execute() = %d, %v", value, err)
		}
	})

	t.Run("FD-2 Execute records the result", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		for range 4 {
			_, _ = Execute(context.Background(), b, func(context.Context) (int, error) { return 0, errDependency })
		}
		assertState(t, b, Open)
	})
}

type retry int

func (n retry) Do(ctx context.Context, call func(context.Context) error) error {
	var err error
	for range int(n) {
		if err = call(ctx); err == nil || errors.Is(err, faultvane.ErrRejected) {
			return err
		}
	}
	return err
}

func TestPipeline(t *testing.T) {
	t.Run("TK-2 TK-3 PI-2 a breaker outside a retry counts one request as one call", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		pipeline := faultvane.Combine(b, retry(3))
		target := &dependency{}
		err := pipeline.Do(context.Background(), target.respond(errDependency))
		calls, failures, _ := b.current.Load().window.totals(0)
		if err != errDependency || target.calls.Load() != 3 || calls != 1 || failures != 1 {
			t.Fatalf("err = %v, attempts = %d, calls = %d, failures = %d, want 3 attempts, 1 call, 1 failure", err, target.calls.Load(), calls, failures)
		}
	})

	t.Run("TK-2 TK-3 a breaker inside a retry counts each attempt and stops the retry when it opens", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		pipeline := faultvane.Combine(retry(10), b)
		target := &dependency{}
		err := pipeline.Do(context.Background(), target.respond(errDependency))
		if err != ErrCircuitOpen || target.calls.Load() != 4 {
			t.Fatalf("err = %v, attempts = %d, want ErrCircuitOpen after 4 attempts", err, target.calls.Load())
		}
	})

	t.Run("TK-2 Execute of the core works with a breaker", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(b), func(context.Context) (int, error) { return 3, nil })
		if value != 3 || err != nil {
			t.Fatalf("Execute() = %d, %v", value, err)
		}
	})
}

func TestConcurrency(t *testing.T) {
	const goroutines = 64

	together := func(work func(id int)) {
		var group sync.WaitGroup
		begin := make(chan struct{})
		for id := range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				<-begin
				work(id)
			}()
		}
		close(begin)
		group.Wait()
	}

	t.Run("API-2 TK-14 PF-4 concurrent successful calls are all counted", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		target := &dependency{}
		together(func(int) {
			for range 500 {
				_ = b.Do(context.Background(), target.respond(nil))
			}
		})
		calls, failures, _ := b.current.Load().window.totals(0)
		if calls != goroutines*500 || failures != 0 || target.calls.Load() != goroutines*500 {
			t.Fatalf("calls = %d, failures = %d, dependency calls = %d", calls, failures, target.calls.Load())
		}
		assertState(t, b, Closed)
	})

	t.Run("API-2 GEN-1 concurrent failures open the breaker one time", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		together(func(int) {
			for range 200 {
				_ = run(b, errDependency)
			}
		})
		assertState(t, b, Open)
		if got := generation(b); got != 1 {
			t.Fatalf("generation = %d, want 1", got)
		}
	})

	t.Run("API-2 SM-5 HO-4 concurrent probe requests get exactly the probe limit", func(t *testing.T) {
		for _, limit := range []int{1, 3} {
			b, clock := newTestBreaker(t, WithHalfOpenMaxProbes(limit), WithHalfOpenSuccessThreshold(limit+1))
			openBreaker(t, b)
			clock.Advance(testOpenDuration)
			var granted, notPermitted atomic.Int64
			together(func(int) {
				switch _, err := b.Allow(context.Background()); err {
				case nil:
					granted.Add(1)
				case ErrProbeNotPermitted:
					notPermitted.Add(1)
				}
			})
			if granted.Load() != int64(limit) || notPermitted.Load() != int64(goroutines-limit) {
				t.Fatalf("limit %d: granted = %d, not permitted = %d", limit, granted.Load(), notPermitted.Load())
			}
			if got := generation(b); got != 2 {
				t.Fatalf("generation = %d, want 2", got)
			}
		}
	})

	t.Run("API-2 concurrent calls during transitions keep a valid state", func(t *testing.T) {
		b, clock := newTestBreaker(t)
		together(func(id int) {
			for i := range 300 {
				var result error
				if (id+i)%3 == 0 {
					result = errDependency
				}
				_ = run(b, result)
				if i%50 == 0 {
					clock.Advance(testOpenDuration)
				}
			}
		})
		if state := b.State(); state != Closed && state != Open && state != HalfOpen {
			t.Fatalf("State() = %v", state)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		view := b.current.Load()
		if view.state != b.record.State || view.generation != b.record.Generation {
			t.Fatalf("snapshot %v/%d differs from machine %v/%d", view.state, view.generation, b.record.State, b.record.Generation)
		}
	})
}

func TestResources(t *testing.T) {
	t.Run("API-4 TK-15 PF-6 a breaker starts no goroutine", func(t *testing.T) {
		before := runtime.NumGoroutine()
		b, clock := newTestBreaker(t)
		openBreaker(t, b)
		clock.Advance(testOpenDuration)
		runTimes(t, b, 5, nil)
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("API-4 TK-15 the breaker code starts a goroutine only for the synchronizer", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		var found []string
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			ast.Inspect(file, func(node ast.Node) bool {
				if _, isGo := node.(*ast.GoStmt); isGo {
					found = append(found, entry.Name())
				}
				return true
			})
		}
		if checked == 0 || !slices.Equal(found, []string{"sync.go"}) {
			t.Fatalf("checked %d files, go statements in %v, want one in sync.go", checked, found)
		}
	})

	t.Run("TK-50 PF-2 a call on a closed breaker makes no allocation", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = b.Do(ctx, call) }); allocations != 0 {
			t.Errorf("Do: %v allocations for each call, want 0", allocations)
		}
		typed := func(context.Context) (int, error) { return 1, nil }
		if allocations := testing.AllocsPerRun(1000, func() { _, _ = Execute(ctx, b, typed) }); allocations != 0 {
			t.Errorf("Execute: %v allocations for each call, want 0", allocations)
		}
		if allocations := testing.AllocsPerRun(1000, func() {
			permit, _ := b.Allow(ctx)
			permit.Success()
		}); allocations != 0 {
			t.Errorf("Allow: %v allocations for each call, want 0", allocations)
		}
	})

	t.Run("TK-50 a rejection of an open breaker makes no allocation", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		openBreaker(t, b)
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = b.Do(ctx, call) }); allocations != 0 {
			t.Fatalf("%v allocations for each call, want 0", allocations)
		}
	})

	t.Run("API-5 the breaker works with the system clock", func(t *testing.T) {
		b, err := New("system-clock", WithLogger(quiet), WithMinimumCalls(1), WithOpenDuration(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		runTimes(t, b, 1, errDependency)
		assertState(t, b, Open)
		assertRejected(t, b, ErrCircuitOpen)
	})
}
