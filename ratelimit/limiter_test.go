package ratelimit

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
	"github.com/faultvane/faultvane-go/faultvanetest"
)

var (
	testStart     = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	errDependency = errors.New("dependency failed")
)

func newTestLimiter(t testing.TB, opts ...Option) (*Limiter, *faultvanetest.Clock) {
	t.Helper()
	clock := faultvanetest.NewClock(testStart)
	limiter, err := New(append([]Option{WithClock(clock), WithRate(10), WithBurst(5)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return limiter, clock
}

func permitted(limiter *Limiter) int {
	count := 0
	for limiter.Allow() {
		count++
	}
	return count
}

func assertPermitted(t *testing.T, limiter *Limiter, want int) {
	t.Helper()
	if got := permitted(limiter); got != want {
		t.Fatalf("permitted calls = %d, want %d", got, want)
	}
}

func TestDo(t *testing.T) {
	t.Run("RL-1 RL-2 TK-2 a call with a token runs one time", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		calls := 0
		err := faultvane.Policy(limiter).Do(context.Background(), func(context.Context) error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, calls)
		}
	})

	t.Run("RL-2 TK-12 the error of the call returns unchanged", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		if err := limiter.Do(context.Background(), func(context.Context) error { return errDependency }); err != errDependency {
			t.Fatalf("Do() = %v, want %v", err, errDependency)
		}
	})

	t.Run("RL-2 the call receives the context of the caller", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		type key struct{}
		ctx := context.WithValue(context.Background(), key{}, "value")
		_ = limiter.Do(ctx, func(received context.Context) error {
			if received != ctx {
				t.Error("the call received a different context")
			}
			return nil
		})
	})

	t.Run("RL-2 each call uses one token", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		for range 2 {
			_ = limiter.Do(context.Background(), func(context.Context) error { return errDependency })
		}
		assertPermitted(t, limiter, 3)
	})

	t.Run("RL-3 a call without a token is rejected and does not run", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		calls := 0
		err := limiter.Do(context.Background(), func(context.Context) error {
			calls++
			return nil
		})
		if err != ErrRateLimited || calls != 0 {
			t.Fatalf("Do() = %v with %d calls, want ErrRateLimited with 0 calls", err, calls)
		}
	})

	t.Run("RL-4 TK-11 the rejection matches the common rejection error", func(t *testing.T) {
		if !errors.Is(ErrRateLimited, faultvane.ErrRejected) {
			t.Fatal("ErrRateLimited does not match faultvane.ErrRejected")
		}
		if !strings.HasPrefix(ErrRateLimited.Error(), "ratelimit: ") {
			t.Fatalf("message = %q", ErrRateLimited)
		}
	})

	t.Run("RL-1 the limiter works in a pipeline", func(t *testing.T) {
		limiter, _ := newTestLimiter(t, WithBurst(1))
		pipeline := faultvane.Combine(limiter)
		value, err := faultvane.Execute(context.Background(), pipeline, func(context.Context) (int, error) { return 7, nil })
		if value != 7 || err != nil {
			t.Fatalf("Execute() = %d, %v, want 7, nil", value, err)
		}
		_, err = faultvane.Execute(context.Background(), pipeline, func(context.Context) (int, error) { return 7, nil })
		if !errors.Is(err, faultvane.ErrRejected) {
			t.Fatalf("Execute() error = %v, want a rejection", err)
		}
	})

	t.Run("RL-13 the limiter works with the system clock", func(t *testing.T) {
		limiter, err := New(WithRate(1e-9), WithBurst(2))
		if err != nil {
			t.Fatal(err)
		}
		assertPermitted(t, limiter, 2)
	})
}

func TestBucket(t *testing.T) {
	t.Run("RL-5 the bucket starts full", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
	})

	t.Run("RL-9 Allow uses one token and reports the decision", func(t *testing.T) {
		limiter, _ := newTestLimiter(t, WithBurst(2))
		if !limiter.Allow() || !limiter.Allow() || limiter.Allow() {
			t.Fatal("Allow() does not permit exactly 2 calls")
		}
	})

	t.Run("RL-6 the bucket refills from the elapsed time", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(300 * time.Millisecond)
		assertPermitted(t, limiter, 3)
	})

	t.Run("RL-6 a token is available at the exact time and not before it", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(100*time.Millisecond - time.Nanosecond)
		assertPermitted(t, limiter, 0)
		clock.Advance(time.Nanosecond)
		assertPermitted(t, limiter, 1)
	})

	t.Run("RL-6 a part of a token stays in the bucket", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(150 * time.Millisecond)
		assertPermitted(t, limiter, 1)
		clock.Advance(50 * time.Millisecond)
		assertPermitted(t, limiter, 1)
	})

	t.Run("RL-6 a rate below one call each second refills slowly", func(t *testing.T) {
		limiter, clock := newTestLimiter(t, WithRate(0.5), WithBurst(1))
		assertPermitted(t, limiter, 1)
		clock.Advance(2*time.Second - time.Nanosecond)
		assertPermitted(t, limiter, 0)
		clock.Advance(time.Nanosecond)
		assertPermitted(t, limiter, 1)
	})

	t.Run("RL-7 the bucket never holds more than the burst", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		clock.Advance(time.Hour)
		assertPermitted(t, limiter, 5)
		clock.Advance(500 * time.Millisecond)
		clock.Advance(time.Hour)
		assertPermitted(t, limiter, 5)
	})

	t.Run("RL-7 a full bucket keeps no time credit", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		clock.Advance(time.Hour)
		if !limiter.Allow() {
			t.Fatal("Allow() = false on a full bucket")
		}
		assertPermitted(t, limiter, 4)
		clock.Advance(100*time.Millisecond - time.Nanosecond)
		assertPermitted(t, limiter, 0)
	})

	t.Run("RL-7 a bucket that becomes exactly full refills from that time", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(500 * time.Millisecond)
		if !limiter.Allow() {
			t.Fatal("Allow() = false on a full bucket")
		}
		clock.Advance(50 * time.Millisecond)
		assertPermitted(t, limiter, 4)
		clock.Advance(50 * time.Millisecond)
		assertPermitted(t, limiter, 1)
	})

	t.Run("RL-8 many small clock steps cause no drift", func(t *testing.T) {
		for _, rate := range []float64{3, 7, 10, 1000} {
			limiter, clock := newTestLimiter(t, WithRate(rate), WithBurst(2000))
			assertPermitted(t, limiter, 2000)
			for range 100_000 {
				clock.Advance(10 * time.Microsecond)
			}
			assertPermitted(t, limiter, int(rate))
		}
	})

	t.Run("RL-8 calls between small clock steps cause no drift", func(t *testing.T) {
		limiter, clock := newTestLimiter(t, WithRate(1000), WithBurst(1))
		assertPermitted(t, limiter, 1)
		total := 0
		for step := 1; step <= 100_000; step++ {
			clock.Advance(100 * time.Microsecond)
			allowed := limiter.Allow()
			if allowed != (step%10 == 0) {
				t.Fatalf("step %d: Allow() = %v", step, allowed)
			}
			if allowed {
				total++
			}
		}
		if total != 10_000 {
			t.Fatalf("permitted calls = %d in 10 seconds, want 10000", total)
		}
	})

	t.Run("RL-10 a rejected call uses no token", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(90 * time.Millisecond)
		for range 100 {
			if err := limiter.Do(context.Background(), func(context.Context) error { return nil }); err != ErrRateLimited {
				t.Fatalf("Do() = %v, want ErrRateLimited", err)
			}
		}
		clock.Advance(10 * time.Millisecond)
		assertPermitted(t, limiter, 1)
	})
}

func TestContext(t *testing.T) {
	t.Run("RL-15 TK-13 a context that has ended returns its error and uses no token", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		expired, release := context.WithDeadline(context.Background(), testStart)
		defer release()
		for ctx, want := range map[context.Context]error{canceled: context.Canceled, expired: context.DeadlineExceeded} {
			calls := 0
			err := limiter.Do(ctx, func(context.Context) error {
				calls++
				return nil
			})
			if err != want || calls != 0 {
				t.Fatalf("Do() = %v with %d calls, want %v with 0 calls", err, calls, want)
			}
		}
		assertPermitted(t, limiter, 5)
	})

	t.Run("RL-15 a context that ends during the call does not change the result", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		ctx, cancel := context.WithCancel(context.Background())
		err := limiter.Do(ctx, func(context.Context) error {
			cancel()
			return errDependency
		})
		if err != errDependency {
			t.Fatalf("Do() = %v, want %v", err, errDependency)
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("RL-17 TK-17 a panic of the call continues with the same value", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		recovered := func() (value any) {
			defer func() { value = recover() }()
			_ = limiter.Do(context.Background(), func(context.Context) error { panic("broken") })
			return nil
		}()
		if recovered != "broken" {
			t.Fatalf("recovered = %v, want broken", recovered)
		}
		assertPermitted(t, limiter, 4)
	})
}

func TestConcurrency(t *testing.T) {
	const goroutines = 64

	together := func(work func()) {
		var group sync.WaitGroup
		begin := make(chan struct{})
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				<-begin
				work()
			}()
		}
		close(begin)
		group.Wait()
	}

	t.Run("RL-16 TK-14 concurrent calls at one instant get exactly the available tokens", func(t *testing.T) {
		limiter, clock := newTestLimiter(t, WithBurst(1000))
		for _, want := range []int64{1000, 250} {
			var allowed, rejected atomic.Int64
			together(func() {
				for range 100 {
					if limiter.Allow() {
						allowed.Add(1)
					} else {
						rejected.Add(1)
					}
				}
			})
			if allowed.Load() != want || rejected.Load() != goroutines*100-want {
				t.Fatalf("allowed = %d, rejected = %d, want %d allowed", allowed.Load(), rejected.Load(), want)
			}
			clock.Advance(25 * time.Second)
			assertPermitted(t, limiter, 250)
			clock.Advance(25 * time.Second)
		}
	})

	t.Run("RL-16 concurrent calls through Do run exactly the permitted number of calls", func(t *testing.T) {
		limiter, _ := newTestLimiter(t, WithBurst(500))
		var calls, rejections atomic.Int64
		together(func() {
			for range 50 {
				err := limiter.Do(context.Background(), func(context.Context) error {
					calls.Add(1)
					return nil
				})
				if err == ErrRateLimited {
					rejections.Add(1)
				}
			}
		})
		if calls.Load() != 500 || rejections.Load() != goroutines*50-500 {
			t.Fatalf("calls = %d, rejections = %d, want 500 and %d", calls.Load(), rejections.Load(), goroutines*50-500)
		}
	})

	t.Run("RL-16 calls during clock steps never exceed the burst and the refill", func(t *testing.T) {
		limiter, clock := newTestLimiter(t, WithRate(1000), WithBurst(100))
		var allowed atomic.Int64
		const steps = 200
		together(func() {
			for range steps {
				clock.Advance(time.Millisecond)
				if limiter.Allow() {
					allowed.Add(1)
				}
			}
		})
		limit := int64(100 + goroutines*steps)
		if got := allowed.Load() + int64(permitted(limiter)); got > limit || allowed.Load() == 0 {
			t.Fatalf("permitted calls = %d, want from 1 to %d", got, limit)
		}
	})
}

func TestResources(t *testing.T) {
	t.Run("RL-18 TK-15 a limiter starts no goroutine", func(t *testing.T) {
		before := runtime.NumGoroutine()
		limiter, clock := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		clock.Advance(time.Second)
		assertPermitted(t, limiter, 5)
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("RL-18 TK-15 the limiter code contains no go statement", func(t *testing.T) {
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

	t.Run("RL-19 TK-50 a permitted call makes no allocation", func(t *testing.T) {
		limiter, _ := newTestLimiter(t, WithBurst(10_000))
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Do(ctx, call) }); allocations != 0 {
			t.Errorf("Do: %v allocations for each call, want 0", allocations)
		}
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Allow() }); allocations != 0 {
			t.Errorf("Allow: %v allocations for each call, want 0", allocations)
		}
		if limiter.Do(ctx, call) != nil {
			t.Fatal("the bucket became empty during the measurement")
		}
	})

	t.Run("RL-19 TK-50 a rejection makes no allocation", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		assertPermitted(t, limiter, 5)
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Do(ctx, call) }); allocations != 0 {
			t.Errorf("Do: %v allocations for each call, want 0", allocations)
		}
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Allow() }); allocations != 0 {
			t.Errorf("Allow: %v allocations for each call, want 0", allocations)
		}
	})
}

func FuzzBucket(f *testing.F) {
	f.Add(10.0, 5, []byte{0, 0, 0, 0, 0, 0, 200, 0, 255, 0, 0})
	f.Add(0.5, 1, []byte{0, 255, 255, 0, 0})
	f.Add(1e9, 3, []byte{1, 0, 0, 0, 0, 2, 0})
	f.Fuzz(func(t *testing.T, rate float64, burst int, steps []byte) {
		clock := faultvanetest.NewClock(testStart)
		limiter, err := New(WithRate(rate), WithBurst(burst), WithClock(clock))
		if err != nil {
			t.Skip()
		}
		refill := 0.0
		allowed := 0
		for _, step := range steps {
			if step == 0 {
				if limiter.Allow() {
					allowed++
				}
			} else {
				advance := time.Duration(step) * time.Duration(step) * time.Millisecond
				clock.Advance(advance)
				refill += advance.Seconds() * rate
			}
			tokens := limiter.available(clock.Now())
			if tokens < 0 || tokens > float64(burst) {
				t.Fatalf("tokens = %v, want from 0 to %d", tokens, burst)
			}
		}
		if limit := float64(burst) + refill; float64(allowed) > limit*(1+1e-9) {
			t.Fatalf("permitted calls = %d, want at most %v", allowed, limit)
		}
	})
}
