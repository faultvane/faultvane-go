package retry

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

var (
	errDependency = errors.New("dependency failed")
	errRejection  = fmt.Errorf("policy rejected the call: %w", faultvane.ErrRejected)
)

type dependency struct {
	calls atomic.Int64
}

func (d *dependency) respond(results ...error) func(context.Context) error {
	return func(context.Context) error {
		call := int(d.calls.Add(1))
		return results[min(call, len(results))-1]
	}
}

type waits struct {
	durations []time.Duration
	ceilings  []int64
}

func newTestRetry(t *testing.T, opts ...Option) (*Retry, *waits) {
	t.Helper()
	r, err := New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	recorded := &waits{}
	r.random = func(ceiling int64) int64 {
		recorded.ceilings = append(recorded.ceilings, ceiling)
		return ceiling - 1
	}
	r.wait = func(_ context.Context, d time.Duration) bool {
		recorded.durations = append(recorded.durations, d)
		return true
	}
	return r, recorded
}

func withoutWaits(t *testing.T, opts ...Option) *Retry {
	t.Helper()
	r, err := New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.wait = func(context.Context, time.Duration) bool { return true }
	return r
}

func assertResult(t *testing.T, got, want error, target *dependency, calls int64) {
	t.Helper()
	if got != want || target.calls.Load() != calls {
		t.Fatalf("Do() = %v with %d calls of the dependency, want %v with %d calls", got, target.calls.Load(), want, calls)
	}
}

func TestAttempts(t *testing.T) {
	t.Run("RT-1 TK-2 the retry is a policy that works in a pipeline and with Execute", func(t *testing.T) {
		r, _ := newTestRetry(t)
		attempts := 0
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(r), func(context.Context) (int, error) {
			attempts++
			if attempts < 3 {
				return 0, errDependency
			}
			return 7, nil
		})
		if value != 7 || err != nil || attempts != 3 {
			t.Fatalf("Execute() = %d, %v with %d attempts, want 7, nil with 3 attempts", value, err, attempts)
		}
	})

	t.Run("RT-3 a successful first attempt returns nil and makes no retry", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(nil)), nil, target, 1)
		if len(recorded.durations) != 0 {
			t.Fatalf("waits = %v, want none", recorded.durations)
		}
	})

	t.Run("RT-3 a successful retry returns nil and stops", func(t *testing.T) {
		r, recorded := newTestRetry(t, WithMaxAttempts(5))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency, nil)), nil, target, 2)
		if len(recorded.durations) != 1 {
			t.Fatalf("waits = %v, want one", recorded.durations)
		}
	})

	t.Run("RT-2 the default is 3 attempts", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 3)
		if len(recorded.durations) != 2 {
			t.Fatalf("waits = %v, want two", recorded.durations)
		}
	})

	for _, attempts := range []int{1, 2, 7} {
		t.Run(fmt.Sprintf("RT-2 the call runs %d times with a limit of %d attempts", attempts, attempts), func(t *testing.T) {
			r, recorded := newTestRetry(t, WithMaxAttempts(attempts))
			target := &dependency{}
			assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, int64(attempts))
			if len(recorded.durations) != attempts-1 {
				t.Fatalf("%d waits, want %d", len(recorded.durations), attempts-1)
			}
		})
	}

	t.Run("RT-4 TK-12 the retry returns the last error of the call without a change", func(t *testing.T) {
		r, _ := newTestRetry(t)
		first, second, last := errors.New("first"), errors.New("second"), errors.New("last")
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(first, second, last)), last, target, 3)
	})

	t.Run("RT-4 each attempt gets the context of the caller", func(t *testing.T) {
		r, _ := newTestRetry(t)
		ctx := context.WithValue(context.Background(), contextKey{}, "value")
		attempts := 0
		_ = r.Do(ctx, func(got context.Context) error {
			attempts++
			if got != ctx {
				t.Errorf("attempt %d got a different context", attempts)
			}
			return errDependency
		})
		if attempts != 3 {
			t.Fatalf("%d attempts, want 3", attempts)
		}
	})
}

type contextKey struct{}

func TestRetryRule(t *testing.T) {
	t.Run("RT-5 TK-11 a rejection is final", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errRejection)), errRejection, target, 1)
		if len(recorded.durations) != 0 {
			t.Fatalf("waits = %v, want none", recorded.durations)
		}
	})

	t.Run("RT-5 a rejection after a failed attempt is final", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(5))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency, faultvane.ErrRejected)), faultvane.ErrRejected, target, 2)
	})

	t.Run("RT-5 a rejection is final with a custom retry rule, and the rule is not asked", func(t *testing.T) {
		asked := 0
		r, _ := newTestRetry(t, WithRetryIf(func(error) bool {
			asked++
			return true
		}))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errRejection)), errRejection, target, 1)
		if asked != 0 {
			t.Fatalf("the retry rule was asked %d times, want 0", asked)
		}
	})

	t.Run("RT-5 a rejection of an outer policy in a pipeline is not tried again", func(t *testing.T) {
		r, _ := newTestRetry(t)
		target := &dependency{}
		pipeline := faultvane.Combine(r, rejecting{})
		assertResult(t, pipeline.Do(context.Background(), target.respond(nil)), errRejection, target, 0)
	})

	t.Run("RT-6 the retry stops when the context of the caller has ended", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		ctx, cancel := context.WithCancel(context.Background())
		target := &dependency{}
		err := r.Do(ctx, func(ctx context.Context) error {
			cancel()
			return target.respond(errDependency)(ctx)
		})
		assertResult(t, err, errDependency, target, 1)
		if len(recorded.durations) != 0 {
			t.Fatalf("waits = %v, want none", recorded.durations)
		}
	})

	t.Run("RT-6 the retry stops when the context ended and the custom retry rule permits a retry", func(t *testing.T) {
		r, _ := newTestRetry(t, WithRetryIf(func(error) bool { return true }))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		target := &dependency{}
		assertResult(t, r.Do(ctx, target.respond(context.Canceled)), context.Canceled, target, 1)
	})

	t.Run("RT-6 the first attempt runs when the context ended before the call", func(t *testing.T) {
		r, _ := newTestRetry(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		target := &dependency{}
		assertResult(t, r.Do(ctx, target.respond(nil)), nil, target, 1)
	})

	t.Run("RT-6 a context error of the call is tried again while the context of the caller is active", func(t *testing.T) {
		r, _ := newTestRetry(t)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(context.DeadlineExceeded, nil)), nil, target, 2)
	})

	t.Run("RT-7 the custom retry rule makes an error final", func(t *testing.T) {
		permanent := errors.New("not found")
		var asked []error
		r, _ := newTestRetry(t, WithMaxAttempts(5), WithRetryIf(func(err error) bool {
			asked = append(asked, err)
			return err != permanent
		}))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency, permanent)), permanent, target, 2)
		if len(asked) != 2 || asked[0] != errDependency || asked[1] != permanent {
			t.Fatalf("the retry rule got %v, want each error of the call", asked)
		}
	})

	t.Run("RT-7 the custom retry rule is not asked after the last attempt or after a success", func(t *testing.T) {
		asked := 0
		r, _ := newTestRetry(t, WithMaxAttempts(2), WithRetryIf(func(error) bool {
			asked++
			return true
		}))
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		if asked != 1 {
			t.Fatalf("the retry rule was asked %d times, want 1", asked)
		}
	})
}

type rejecting struct{}

func (rejecting) Do(context.Context, func(context.Context) error) error { return errRejection }

func TestBackoff(t *testing.T) {
	t.Run("RT-8 the ceiling of the wait doubles from the initial duration to the maximum", func(t *testing.T) {
		r, recorded := newTestRetry(t, WithMaxAttempts(6), WithBackoff(100*time.Millisecond, 500*time.Millisecond))
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		want := []int64{int64(100 * time.Millisecond), int64(200 * time.Millisecond), int64(400 * time.Millisecond), int64(500 * time.Millisecond), int64(500 * time.Millisecond)}
		if fmt.Sprint(recorded.ceilings) != fmt.Sprint(want) {
			t.Fatalf("ceilings = %v, want %v", recorded.ceilings, want)
		}
		for i, d := range recorded.durations {
			if int64(d) != want[i]-1 {
				t.Fatalf("wait %d = %v, want the value of the random source %v", i+1, d, time.Duration(want[i]-1))
			}
		}
	})

	t.Run("RT-8 the default backoff starts at 100ms", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		if want := []int64{int64(100 * time.Millisecond), int64(200 * time.Millisecond)}; fmt.Sprint(recorded.ceilings) != fmt.Sprint(want) {
			t.Fatalf("ceilings = %v, want %v", recorded.ceilings, want)
		}
	})

	t.Run("RT-8 the wait is a random duration from 0 to below the ceiling", func(t *testing.T) {
		r, err := New(WithMaxAttempts(2), WithBackoff(time.Hour, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[time.Duration]bool{}
		r.wait = func(_ context.Context, d time.Duration) bool {
			if d < 0 || d >= time.Hour {
				t.Errorf("wait = %v, want 0 or more and below 1h", d)
			}
			seen[d] = true
			return true
		}
		for range 200 {
			_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		}
		if len(seen) < 100 {
			t.Fatalf("200 waits had %d different durations, want random durations", len(seen))
		}
	})

	t.Run("RT-8 a ceiling of 1ns gives a wait of 0", func(t *testing.T) {
		r := withoutWaits(t, WithBackoff(time.Nanosecond, time.Nanosecond))
		var got []time.Duration
		r.wait = func(_ context.Context, d time.Duration) bool {
			got = append(got, d)
			return true
		}
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		if len(got) != 2 || got[0] != 0 || got[1] != 0 {
			t.Fatalf("waits = %v, want [0s 0s]", got)
		}
	})

	t.Run("RT-9 TK-13 the wait stops when the context ends and the retry returns the last error of the call", func(t *testing.T) {
		r, err := New(WithBackoff(time.Hour, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		r.random = func(ceiling int64) int64 { return ceiling - 1 }
		ctx, cancel := context.WithCancel(context.Background())
		target := &dependency{}
		stop := context.AfterFunc(waiting(r), cancel)
		defer stop()
		assertResult(t, r.Do(ctx, target.respond(errDependency)), errDependency, target, 1)
	})

	t.Run("RT-9 the wait function reports a wait that completed", func(t *testing.T) {
		if !waitFor(context.Background(), 0) || !waitFor(context.Background(), time.Nanosecond) {
			t.Fatal("waitFor() = false, want true")
		}
	})

	t.Run("RT-9 the wait function reports a context that ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range 100 {
			if waitFor(ctx, 0) {
				t.Fatal("waitFor() = true with a context that ended, want false")
			}
		}
		if waitFor(ctx, time.Hour) {
			t.Fatal("waitFor() = true with a context that ended, want false")
		}
	})

	t.Run("RT-9 the real wait lets the next attempt run", func(t *testing.T) {
		r, err := New(WithBackoff(time.Nanosecond, time.Nanosecond))
		if err != nil {
			t.Fatal(err)
		}
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency, errDependency, nil)), nil, target, 3)
	})
}

func waiting(r *Retry) context.Context {
	started, signal := context.WithCancel(context.Background())
	wait := r.wait
	r.wait = func(ctx context.Context, d time.Duration) bool {
		signal()
		return wait(ctx, d)
	}
	return started
}

func TestGate(t *testing.T) {
	t.Run("RT-10 a closed gate stops the retry and returns the last error", func(t *testing.T) {
		r, recorded := newTestRetry(t, WithGate(func() bool { return false }))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 1)
		if len(recorded.durations) != 0 {
			t.Fatalf("waits = %v, want none", recorded.durations)
		}
	})

	t.Run("RT-10 the gate is asked one time before each retry and not before the first attempt", func(t *testing.T) {
		asked := 0
		target := &dependency{}
		var callsAtGate []int64
		r, _ := newTestRetry(t, WithMaxAttempts(4), WithGate(func() bool {
			asked++
			callsAtGate = append(callsAtGate, target.calls.Load())
			return true
		}))
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 4)
		if asked != 3 || fmt.Sprint(callsAtGate) != "[1 2 3]" {
			t.Fatalf("the gate was asked %d times after the calls %v, want 3 times after the calls [1 2 3]", asked, callsAtGate)
		}
	})

	t.Run("RT-10 the gate is not asked for a success, a final error, or the last attempt", func(t *testing.T) {
		asked := 0
		r, _ := newTestRetry(t, WithMaxAttempts(2), WithGate(func() bool {
			asked++
			return true
		}))
		_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		_ = r.Do(context.Background(), (&dependency{}).respond(errRejection))
		if asked != 0 {
			t.Fatalf("the gate was asked %d times, want 0", asked)
		}
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		if asked != 1 {
			t.Fatalf("the gate was asked %d times, want 1", asked)
		}
	})

	t.Run("RT-10 a gate that closes between attempts stops the retry", func(t *testing.T) {
		open := true
		r, _ := newTestRetry(t, WithMaxAttempts(5), WithGate(func() bool { return open }))
		target := &dependency{}
		last := errors.New("last")
		err := r.Do(context.Background(), func(ctx context.Context) error {
			if target.calls.Load() == 1 {
				open = false
				target.calls.Add(1)
				return last
			}
			return target.respond(errDependency)(ctx)
		})
		assertResult(t, err, last, target, 2)
	})

	t.Run("RT-10 a closed gate uses no token of the budget", func(t *testing.T) {
		r, _ := newTestRetry(t, WithGate(func() bool { return false }), WithBudget(1))
		_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		if got := r.budget.units.Load(); got != budgetCap {
			t.Fatalf("budget = %d units, want the full budget", got)
		}
	})
}

func drain(t *testing.T, r *Retry) {
	t.Helper()
	for r.budget.withdraw() {
	}
	if got := r.budget.units.Load(); got != 0 {
		t.Fatalf("budget = %d units after the drain, want 0", got)
	}
}

func retries(r *Retry, requests int) int64 {
	target := &dependency{}
	for range requests {
		_ = r.Do(context.Background(), target.respond(errDependency))
	}
	return target.calls.Load() - int64(requests)
}

func TestBudget(t *testing.T) {
	t.Run("RT-11 without a budget the number of retries has no limit", func(t *testing.T) {
		r, _ := newTestRetry(t)
		if got := retries(r, 1000); got != 2000 {
			t.Fatalf("%d retries for 1000 failed requests, want 2000", got)
		}
	})

	t.Run("RT-12 a new budget has 10 tokens", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(0.001))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 11)
	})

	t.Run("RT-12 each first attempt adds the ratio and each retry removes one token", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(2), WithBudget(0.1))
		drain(t, r)
		for round := range 3 {
			if got := retries(r, 9); got != 0 {
				t.Fatalf("round %d: %d retries for 9 failed requests, want 0", round, got)
			}
			target := &dependency{}
			assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 2)
		}
	})

	t.Run("RT-12 during an outage the retries are the ratio of the requests", func(t *testing.T) {
		r, _ := newTestRetry(t, WithBudget(0.25))
		drain(t, r)
		if got := retries(r, 1000); got != 250 {
			t.Fatalf("%d retries for 1000 failed requests, want 250", got)
		}
	})

	t.Run("RT-12 a successful first attempt adds the ratio", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(0.5))
		drain(t, r)
		for range 4 {
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		}
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 3)
	})

	t.Run("RT-12 a retry adds no token", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(1))
		drain(t, r)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 2)
	})

	t.Run("RT-12 the budget keeps at most 10 tokens", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(1))
		drain(t, r)
		for range 9 {
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		}
		if got := r.budget.units.Load(); got != 9*unitsPerToken {
			t.Fatalf("budget = %d units after 9 requests, want 9 tokens", got)
		}
		for range 50 {
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		}
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 11)
	})

	t.Run("RT-12 a ratio that does not divide the cap fills the budget to exactly 10 tokens", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(0.3))
		drain(t, r)
		for range 34 {
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		}
		if got := r.budget.units.Load(); got != budgetCap {
			t.Fatalf("budget = %d units, want the cap %d", got, budgetCap)
		}
	})

	t.Run("RT-12 the smallest ratio adds more than 0", func(t *testing.T) {
		r, _ := newTestRetry(t, WithBudget(5e-324))
		drain(t, r)
		_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		if got := r.budget.units.Load(); got != 1 {
			t.Fatalf("budget = %d units, want 1", got)
		}
	})

	t.Run("RT-12 the budget keeps the ratio with a precision of one billionth of a token", func(t *testing.T) {
		for ratio, want := range map[float64]int64{1.25e-9: 1, 1.75e-9: 2, 0.3: 300_000_000, 1: unitsPerToken} {
			r, _ := newTestRetry(t, WithBudget(ratio))
			drain(t, r)
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
			if got := r.budget.units.Load(); got != want {
				t.Errorf("a first attempt added %d units for the ratio %v, want %d", got, ratio, want)
			}
		}
	})

	t.Run("RT-13 without a token the retry stops and returns the last error", func(t *testing.T) {
		r, recorded := newTestRetry(t, WithBudget(0.5))
		drain(t, r)
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 1)
		if len(recorded.durations) != 0 {
			t.Fatalf("waits = %v, want none", recorded.durations)
		}
	})

	t.Run("RT-13 exactly one token permits one retry", func(t *testing.T) {
		r, _ := newTestRetry(t, WithMaxAttempts(100), WithBudget(0.5))
		drain(t, r)
		_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency)), errDependency, target, 2)
		if got := r.budget.units.Load(); got != 0 {
			t.Fatalf("budget = %d units, want 0", got)
		}
	})

	t.Run("RT-13 a final error uses no token", func(t *testing.T) {
		r, _ := newTestRetry(t, WithBudget(1))
		_ = r.Do(context.Background(), (&dependency{}).respond(errRejection))
		if got := r.budget.units.Load(); got != budgetCap {
			t.Fatalf("budget = %d units, want the full budget", got)
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("RT-18 TK-17 a panic of the call continues to the caller and the call is not tried again", func(t *testing.T) {
		r, recorded := newTestRetry(t)
		calls := 0
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = r.Do(context.Background(), func(context.Context) error {
				calls++
				panic("call failed")
			})
		}()
		if recovered != "call failed" || calls != 1 || len(recorded.durations) != 0 {
			t.Fatalf("recovered %v after %d calls and %d waits, want the value of the panic after 1 call", recovered, calls, len(recorded.durations))
		}
	})

	t.Run("RT-18 the retry works after a panic", func(t *testing.T) {
		r, _ := newTestRetry(t)
		func() {
			defer func() { _ = recover() }()
			_ = r.Do(context.Background(), func(context.Context) error { panic("call failed") })
		}()
		target := &dependency{}
		assertResult(t, r.Do(context.Background(), target.respond(errDependency, nil)), nil, target, 2)
	})
}

func TestConcurrency(t *testing.T) {
	const goroutines, requests = 32, 200

	t.Run("RT-16 TK-14 many goroutines use one retry", func(t *testing.T) {
		r := withoutWaits(t)
		target := &dependency{}
		var failures atomic.Int64
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range requests {
					if r.Do(context.Background(), target.respond(errDependency)) == errDependency {
						failures.Add(1)
					}
				}
			}()
		}
		group.Wait()
		if failures.Load() != goroutines*requests || target.calls.Load() != 3*goroutines*requests {
			t.Fatalf("%d failures with %d calls, want %d failures with %d calls", failures.Load(), target.calls.Load(), goroutines*requests, 3*goroutines*requests)
		}
	})

	t.Run("RT-16 the budget holds exactly under contention", func(t *testing.T) {
		r := withoutWaits(t, WithMaxAttempts(2), WithBudget(0.25))
		drain(t, r)
		target := &dependency{}
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range requests {
					_ = r.Do(context.Background(), target.respond(errDependency))
				}
			}()
		}
		group.Wait()
		retried := target.calls.Load() - goroutines*requests
		left := r.budget.units.Load()
		if retried > goroutines*requests/4 || retried*unitsPerToken+left != goroutines*requests*unitsPerToken/4 {
			t.Fatalf("%d retries and %d units left for %d requests, want all added tokens used or left and at most %d retries", retried, left, goroutines*requests, goroutines*requests/4)
		}
		if left >= unitsPerToken {
			t.Fatalf("%d units left, want less than one token", left)
		}
	})
}

func TestGoroutinesAndAllocations(t *testing.T) {
	t.Run("RT-17 TK-15 the retry leaves no goroutine", func(t *testing.T) {
		r, err := New(WithBackoff(time.Nanosecond, time.Nanosecond), WithBudget(1))
		if err != nil {
			t.Fatal(err)
		}
		before := runtime.NumGoroutine()
		for range 100 {
			_ = r.Do(context.Background(), (&dependency{}).respond(errDependency))
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("RT-17 TK-15 the retry code contains no go statement", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
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
				if _, found := node.(*ast.GoStmt); found {
					t.Errorf("%s contains a go statement", entry.Name())
				}
				return true
			})
		}
		if checked == 0 {
			t.Fatal("the test found no source file")
		}
	})

	t.Run("RT-19 TK-50 a successful first attempt makes no allocation", func(t *testing.T) {
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		for name, opts := range map[string][]Option{"without a budget": nil, "with a budget": {WithBudget(0.1)}} {
			r := withoutWaits(t, opts...)
			if allocations := testing.AllocsPerRun(1000, func() { _ = r.Do(ctx, call) }); allocations != 0 {
				t.Errorf("%s: %v allocations for each call, want 0", name, allocations)
			}
		}
	})

	t.Run("RT-19 the decisions of a failed request make no allocation", func(t *testing.T) {
		r := withoutWaits(t, WithBudget(1))
		ctx := context.Background()
		call := func(context.Context) error { return errDependency }
		if allocations := testing.AllocsPerRun(1000, func() { _ = r.Do(ctx, call) }); allocations != 0 {
			t.Fatalf("%v allocations for each call, want 0", allocations)
		}
	})

	t.Run("RT-20 TK-16 the budget has a fixed size", func(t *testing.T) {
		r := withoutWaits(t, WithBudget(1))
		for range 10000 {
			_ = r.Do(context.Background(), (&dependency{}).respond(nil))
		}
		if got := r.budget.units.Load(); got != budgetCap {
			t.Fatalf("budget = %d units, want the cap %d", got, budgetCap)
		}
	})
}
