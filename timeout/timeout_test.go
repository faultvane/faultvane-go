package timeout

import (
	"context"
	"errors"
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

const maxAllocationsForEachCall = 4

var errDependency = errors.New("dependency failed")

type dependencyError struct{ code int }

func (e *dependencyError) Error() string { return "dependency failed with a code" }

type contextKey struct{}

func newTimeout(t *testing.T, d time.Duration) *Timeout {
	t.Helper()
	policy, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func untilEnd(result error) func(context.Context) error {
	return func(ctx context.Context) error {
		<-ctx.Done()
		return result
	}
}

func TestConfiguration(t *testing.T) {
	invalid := map[string]time.Duration{"0": 0, "below 0": -time.Nanosecond, "the smallest duration": -1 << 63}
	for name, d := range invalid {
		t.Run("TO-2 TK-21 a duration of "+name+" is not valid", func(t *testing.T) {
			policy, err := New(d)
			if policy != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", policy, err)
			}
			if want := "faultvane: invalid configuration: duration must be above 0"; err.Error() != want {
				t.Fatalf("err = %q, want %q", err, want)
			}
		})
	}

	valid := map[string]time.Duration{"1ns": time.Nanosecond, "the largest duration": 1<<63 - 1}
	for name, d := range valid {
		t.Run("TO-2 a duration of "+name+" is valid", func(t *testing.T) {
			policy, err := New(d)
			if policy == nil || err != nil {
				t.Fatalf("New() = %v, %v, want a policy and nil", policy, err)
			}
		})
	}
}

func FuzzNew(f *testing.F) {
	for _, seed := range []int64{-1 << 63, -1, 0, 1, int64(time.Second), 1<<63 - 1} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, nanoseconds int64) {
		policy, err := New(time.Duration(nanoseconds))
		if valid := nanoseconds > 0; valid != (err == nil) || valid != (policy != nil) || valid == errors.Is(err, faultvane.ErrInvalidConfig) {
			t.Fatalf("New(%d) = %v, %v", nanoseconds, policy, err)
		}
	})
}

func TestResult(t *testing.T) {
	t.Run("TO-1 TK-2 the timeout is a policy that works in a pipeline and with Execute", func(t *testing.T) {
		pipeline := faultvane.Combine(newTimeout(t, time.Hour))
		value, err := faultvane.Execute(context.Background(), pipeline, func(context.Context) (int, error) { return 7, nil })
		if value != 7 || err != nil {
			t.Fatalf("Execute() = %d, %v, want 7, nil", value, err)
		}
	})

	t.Run("TO-3 the call runs one time", func(t *testing.T) {
		calls := 0
		err := newTimeout(t, time.Hour).Do(context.Background(), func(context.Context) error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, calls)
		}
	})

	t.Run("TO-3 the call runs one time when its time limit ends", func(t *testing.T) {
		calls := 0
		_ = newTimeout(t, time.Nanosecond).Do(context.Background(), func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return errDependency
		})
		if calls != 1 {
			t.Fatalf("%d calls, want 1", calls)
		}
	})

	t.Run("TO-6 a call that returns nil gives nil", func(t *testing.T) {
		if err := newTimeout(t, time.Hour).Do(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Do() = %v, want nil", err)
		}
	})

	t.Run("TO-6 a call that returns nil after the time limit gives nil", func(t *testing.T) {
		if err := newTimeout(t, time.Nanosecond).Do(context.Background(), untilEnd(nil)); err != nil {
			t.Fatalf("Do() = %v, want nil", err)
		}
	})

	t.Run("TO-7 an error after the time limit matches ErrTimeout and the error of the call", func(t *testing.T) {
		err := newTimeout(t, time.Nanosecond).Do(context.Background(), untilEnd(errDependency))
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, errDependency) {
			t.Fatalf("Do() = %v, want a match for ErrTimeout and for the error of the call", err)
		}
		if want := "timeout: call exceeded the time limit: dependency failed"; err.Error() != want {
			t.Fatalf("err = %q, want %q", err, want)
		}
	})

	t.Run("TO-7 the context error of the call matches after the time limit", func(t *testing.T) {
		err := newTimeout(t, time.Nanosecond).Do(context.Background(), func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Do() = %v, want a match for ErrTimeout and for DeadlineExceeded", err)
		}
	})

	t.Run("TO-7 the typed error of the call is available after the time limit", func(t *testing.T) {
		cause := &dependencyError{code: 503}
		err := newTimeout(t, time.Nanosecond).Do(context.Background(), untilEnd(cause))
		var typed *dependencyError
		if !errors.As(err, &typed) || typed != cause || !errors.Is(err, ErrTimeout) {
			t.Fatalf("Do() = %v, want a match for ErrTimeout and the typed error of the call", err)
		}
	})

	t.Run("TO-8 TK-12 an error before the time limit is not changed", func(t *testing.T) {
		if err := newTimeout(t, time.Hour).Do(context.Background(), func(context.Context) error { return errDependency }); err != errDependency {
			t.Fatalf("Do() = %v, want the error of the call", err)
		}
	})

	t.Run("TO-10 TK-11 ErrTimeout is not a rejection", func(t *testing.T) {
		err := newTimeout(t, time.Nanosecond).Do(context.Background(), untilEnd(errDependency))
		if errors.Is(err, faultvane.ErrRejected) || errors.Is(ErrTimeout, faultvane.ErrRejected) {
			t.Fatalf("Do() = %v, want no match for ErrRejected", err)
		}
		if errors.Is(err, faultvane.ErrInvalidConfig) {
			t.Fatalf("Do() = %v, want no match for ErrInvalidConfig", err)
		}
	})
}

func TestContext(t *testing.T) {
	t.Run("TO-4 the call gets a context with the time limit and the values of the caller", func(t *testing.T) {
		parent := context.WithValue(context.Background(), contextKey{}, "value")
		before := time.Now()
		err := newTimeout(t, time.Hour).Do(parent, func(ctx context.Context) error {
			deadline, found := ctx.Deadline()
			if !found || deadline.Before(before.Add(time.Hour)) || deadline.After(time.Now().Add(time.Hour)) {
				t.Errorf("Deadline() = %v, %v, want the start time plus 1h", deadline, found)
			}
			if ctx.Value(contextKey{}) != "value" {
				t.Error("the context of the call does not have the value of the caller")
			}
			if ctx.Err() != nil {
				t.Errorf("Err() = %v during the call, want nil", ctx.Err())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("TO-4 the context of the call ends with DeadlineExceeded at the time limit", func(t *testing.T) {
		var seen error
		_ = newTimeout(t, time.Nanosecond).Do(context.Background(), func(ctx context.Context) error {
			<-ctx.Done()
			seen = ctx.Err()
			return nil
		})
		if seen != context.DeadlineExceeded {
			t.Fatalf("Err() = %v, want DeadlineExceeded", seen)
		}
	})

	t.Run("TO-5 the time limit does not extend the deadline of the caller", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		want, _ := parent.Deadline()
		_ = newTimeout(t, time.Hour).Do(parent, func(ctx context.Context) error {
			if got, _ := ctx.Deadline(); !got.Equal(want) {
				t.Errorf("Deadline() = %v, want the deadline of the caller %v", got, want)
			}
			return nil
		})
	})

	t.Run("TO-5 a time limit before the deadline of the caller applies", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		err := newTimeout(t, time.Nanosecond).Do(parent, untilEnd(errDependency))
		if !errors.Is(err, ErrTimeout) || parent.Err() != nil {
			t.Fatalf("Do() = %v with the caller context error %v, want ErrTimeout and nil", err, parent.Err())
		}
	})

	t.Run("TO-9 TK-13 a cancellation by the caller reaches the call and is not a timeout", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		err := newTimeout(t, time.Hour).Do(parent, func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		})
		if err != context.Canceled {
			t.Fatalf("Do() = %v, want Canceled without a change", err)
		}
	})

	t.Run("TO-9 a context that ended before the call is not a timeout", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		if err := newTimeout(t, time.Hour).Do(parent, untilEnd(errDependency)); err != errDependency {
			t.Fatalf("Do() = %v, want the error of the call without a change", err)
		}
	})

	t.Run("TO-9 the deadline of the caller is not a timeout of the policy", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		err := newTimeout(t, time.Hour).Do(parent, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if err != context.DeadlineExceeded {
			t.Fatalf("Do() = %v, want DeadlineExceeded without a change", err)
		}
	})

	t.Run("TO-9 a caller context that ends after the time limit is not a timeout", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		err := newTimeout(t, time.Nanosecond).Do(parent, func(ctx context.Context) error {
			<-ctx.Done()
			cancel()
			return errDependency
		})
		if err != errDependency {
			t.Fatalf("Do() = %v, want the error of the call without a change", err)
		}
	})

	t.Run("TO-12 the context of the call ends when Do returns", func(t *testing.T) {
		var seen context.Context
		_ = newTimeout(t, time.Hour).Do(context.Background(), func(ctx context.Context) error {
			seen = ctx
			return nil
		})
		if seen.Err() != context.Canceled {
			t.Fatalf("Err() = %v after Do, want Canceled", seen.Err())
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("TO-13 TK-17 a panic of the call continues to the caller and the context of the call ends", func(t *testing.T) {
		var seen context.Context
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = newTimeout(t, time.Hour).Do(context.Background(), func(ctx context.Context) error {
				seen = ctx
				panic("call failed")
			})
		}()
		if recovered != "call failed" {
			t.Fatalf("recovered %v, want the value of the panic", recovered)
		}
		if seen.Err() != context.Canceled {
			t.Fatalf("Err() = %v after the panic, want Canceled", seen.Err())
		}
	})
}

func TestConcurrency(t *testing.T) {
	t.Run("TO-14 TK-14 many goroutines use one timeout", func(t *testing.T) {
		fast := newTimeout(t, time.Hour)
		slow := newTimeout(t, time.Nanosecond)
		var timeouts, failures, successes atomic.Int64
		var group sync.WaitGroup
		for range 32 {
			group.Add(1)
			go func() {
				defer group.Done()
				for range 50 {
					if errors.Is(slow.Do(context.Background(), untilEnd(errDependency)), ErrTimeout) {
						timeouts.Add(1)
					}
					if fast.Do(context.Background(), func(context.Context) error { return errDependency }) == errDependency {
						failures.Add(1)
					}
					if fast.Do(context.Background(), func(context.Context) error { return nil }) == nil {
						successes.Add(1)
					}
				}
			}()
		}
		group.Wait()
		if timeouts.Load() != 1600 || failures.Load() != 1600 || successes.Load() != 1600 {
			t.Fatalf("timeouts = %d, failures = %d, successes = %d, want 1600 each", timeouts.Load(), failures.Load(), successes.Load())
		}
	})
}

func TestGoroutinesAndAllocations(t *testing.T) {
	t.Run("TO-11 the call runs in the goroutine of the caller", func(t *testing.T) {
		identity := func() string {
			buffer := make([]byte, 64)
			return strings.Fields(string(buffer[:runtime.Stack(buffer, false)]))[1]
		}
		caller, inCall := identity(), ""
		_ = newTimeout(t, time.Hour).Do(context.Background(), func(context.Context) error {
			inCall = identity()
			return nil
		})
		if inCall != caller {
			t.Fatalf("the call ran in goroutine %s, want goroutine %s", inCall, caller)
		}
	})

	t.Run("TO-11 TK-15 the timeout leaves no goroutine", func(t *testing.T) {
		policy := newTimeout(t, time.Nanosecond)
		before := runtime.NumGoroutine()
		for range 100 {
			_ = policy.Do(context.Background(), untilEnd(errDependency))
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("TO-11 TK-15 the timeout code contains no go statement", func(t *testing.T) {
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

	t.Run("TO-15 a call makes a small fixed number of allocations", func(t *testing.T) {
		policy := newTimeout(t, time.Hour)
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = policy.Do(ctx, call) }); allocations > maxAllocationsForEachCall {
			t.Fatalf("%v allocations for each call, want %d or fewer", allocations, maxAllocationsForEachCall)
		}
	})
}
