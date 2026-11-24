package concurrencylimit

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand/v2"
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
	errDependency = errors.New("dependency failed")
	testStart     = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func newLimiter(t testing.TB, opts ...Option) (*Limiter, *faultvanetest.Clock) {
	t.Helper()
	clock := faultvanetest.NewClock(testStart)
	l, err := New(append([]Option{WithClock(clock)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return l, clock
}

func fixedLimit(calls int) []Option {
	return []Option{WithMinLimit(calls), WithInitialLimit(calls), WithMaxLimit(calls)}
}

func succeed(context.Context) error { return nil }

func run(l *Limiter, result error) error {
	return l.Do(context.Background(), func(context.Context) error { return result })
}

func runTimes(t *testing.T, l *Limiter, times int, result error) {
	t.Helper()
	for range times {
		if err := run(l, result); err != result {
			t.Fatalf("Do() = %v, want %v", err, result)
		}
	}
}

func holding(l *Limiter, calls int, body func()) {
	if calls == 0 {
		body()
		return
	}
	_ = l.Do(context.Background(), func(context.Context) error {
		holding(l, calls-1, body)
		return nil
	})
}

func assertLimit(t *testing.T, l *Limiter, want int) {
	t.Helper()
	if got := l.Limit(); got != want {
		t.Fatalf("Limit() = %d, want %d", got, want)
	}
}

func assertRejected(t *testing.T, l *Limiter) {
	t.Helper()
	calls := 0
	err := l.Do(context.Background(), func(context.Context) error {
		calls++
		return nil
	})
	if err != ErrLimitExceeded || calls != 0 {
		t.Fatalf("Do() = %v with %d calls, want ErrLimitExceeded with 0 calls", err, calls)
	}
}

func TestCalls(t *testing.T) {
	t.Run("CL-1 CL-18 a call below the limit runs", func(t *testing.T) {
		l, _ := newLimiter(t)
		calls := 0
		err := l.Do(context.Background(), func(context.Context) error {
			calls++
			if l.InFlight() != 1 {
				t.Errorf("InFlight() = %d during the call, want 1", l.InFlight())
			}
			return nil
		})
		if err != nil || calls != 1 || l.InFlight() != 0 {
			t.Fatalf("Do() = %v with %d calls, InFlight() = %d, want nil with 1 call, 0", err, calls, l.InFlight())
		}
		assertLimit(t, l, 20)
	})

	t.Run("CL-12 TK-12 the error of the call returns unchanged", func(t *testing.T) {
		l, _ := newLimiter(t)
		for _, result := range []error{errDependency, context.DeadlineExceeded, context.Canceled} {
			runTimes(t, l, 1, result)
		}
		if l.InFlight() != 0 {
			t.Fatalf("InFlight() = %d, want 0", l.InFlight())
		}
	})

	t.Run("CL-19 TK-2 TK-3 the limiter works in a pipeline", func(t *testing.T) {
		l, _ := newLimiter(t)
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(l), func(context.Context) (int, error) { return 3, nil })
		if value != 3 || err != nil {
			t.Fatalf("Execute() = %d, %v", value, err)
		}
	})
}

func TestDecision(t *testing.T) {
	t.Run("CL-2 a call is rejected when the calls in progress are equal to the limit", func(t *testing.T) {
		l, _ := newLimiter(t, fixedLimit(2)...)
		holding(l, 1, func() { runTimes(t, l, 1, nil) })
		holding(l, 2, func() {
			assertRejected(t, l)
			if l.InFlight() != 2 {
				t.Errorf("InFlight() = %d, want 2", l.InFlight())
			}
		})
		if l.InFlight() != 0 {
			t.Fatalf("InFlight() = %d, want 0", l.InFlight())
		}
		runTimes(t, l, 1, nil)
	})

	t.Run("CL-2 a rejection does not change the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1), WithOverloadIf(func(err error) bool { return err != nil }))
		holding(l, 1, func() {
			for range 10 {
				assertRejected(t, l)
			}
			assertLimit(t, l, 1)
		})
	})

	t.Run("CL-3 a limit that decreases below the calls in progress rejects new calls", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(4), WithBackoffRatio(0.5))
		holding(l, 3, func() {
			runTimes(t, l, 1, context.DeadlineExceeded)
			assertLimit(t, l, 2)
			assertRejected(t, l)
		})
		holding(l, 1, func() { runTimes(t, l, 1, nil) })
	})
}

func TestDecrease(t *testing.T) {
	t.Run("CL-4 CL-7 an overload signal multiplies the limit by the backoff ratio", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5))
		for _, want := range []int{5, 2, 1} {
			runTimes(t, l, 1, context.DeadlineExceeded)
			assertLimit(t, l, want)
		}
	})

	t.Run("CL-4 the default backoff ratio is 0.9", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(100))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertLimit(t, l, 90)
	})

	t.Run("CL-8 the limit does not decrease below MinLimit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithMinLimit(2), WithBackoffRatio(0.5))
		for _, want := range []int{5, 2, 2, 2, 2} {
			runTimes(t, l, 1, context.DeadlineExceeded)
			assertLimit(t, l, want)
		}
		holding(l, 1, func() { runTimes(t, l, 1, context.DeadlineExceeded) })
		holding(l, 2, func() { assertRejected(t, l) })
	})

	t.Run("CL-4 a wrapped deadline error is an overload signal", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5))
		_ = run(l, fmt.Errorf("call: %w", context.DeadlineExceeded))
		assertLimit(t, l, 5)
	})

	t.Run("CL-4 an error that is not an overload signal does not decrease the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5))
		runTimes(t, l, 5, errDependency)
		runTimes(t, l, 5, context.Canceled)
		assertLimit(t, l, 10)
	})

	t.Run("CL-5 OverloadIf selects the errors that are overload signals", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5), WithOverloadIf(func(err error) bool { return err == errDependency }))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertLimit(t, l, 10)
		runTimes(t, l, 1, errDependency)
		assertLimit(t, l, 5)
	})

	t.Run("CL-4 an overload signal of a busy call decreases the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(2), WithBackoffRatio(0.5))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertLimit(t, l, 1)
	})
}

func TestLatency(t *testing.T) {
	take := func(l *Limiter, clock *faultvanetest.Clock, d time.Duration) {
		_ = l.Do(context.Background(), func(context.Context) error {
			clock.Advance(d)
			return nil
		})
	}

	t.Run("CL-6 a call that takes the latency threshold is an overload signal", func(t *testing.T) {
		l, clock := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5), WithLatencyThreshold(100*time.Millisecond))
		take(l, clock, 100*time.Millisecond)
		assertLimit(t, l, 5)
		take(l, clock, time.Hour)
		assertLimit(t, l, 2)
	})

	t.Run("CL-6 a call shorter than the latency threshold is not an overload signal", func(t *testing.T) {
		l, clock := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5), WithLatencyThreshold(100*time.Millisecond))
		for range 5 {
			take(l, clock, 100*time.Millisecond-time.Nanosecond)
		}
		assertLimit(t, l, 10)
	})

	t.Run("CL-6 without a latency threshold a slow call is not an overload signal", func(t *testing.T) {
		l, clock := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5))
		take(l, clock, 24*time.Hour)
		assertLimit(t, l, 10)
	})

	t.Run("CL-6 the time before the call starts does not count", func(t *testing.T) {
		l, clock := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5), WithLatencyThreshold(100*time.Millisecond))
		clock.Advance(time.Hour)
		take(l, clock, time.Millisecond)
		assertLimit(t, l, 10)
	})
}

func TestIncrease(t *testing.T) {
	t.Run("CL-9 a busy call without an overload signal adds 1/limit to the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1))
		for _, want := range []int{2, 2, 2, 3} {
			runTimes(t, l, 1, nil)
			assertLimit(t, l, want)
		}
	})

	t.Run("CL-9 an error that is not an overload signal increases the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1))
		runTimes(t, l, 1, errDependency)
		assertLimit(t, l, 2)
	})

	t.Run("CL-10 a call that starts below half of the limit does not increase the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(3))
		runTimes(t, l, 1000, nil)
		assertLimit(t, l, 3)
		l, _ = newLimiter(t, WithInitialLimit(5))
		for range 1000 {
			holding(l, 1, func() { runTimes(t, l, 1, nil) })
		}
		assertLimit(t, l, 5)
	})

	t.Run("CL-10 a call that starts at half of the limit increases the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(4))
		for range 4 {
			holding(l, 1, func() { runTimes(t, l, 1, nil) })
		}
		assertLimit(t, l, 4)
		holding(l, 1, func() { runTimes(t, l, 1, nil) })
		assertLimit(t, l, 5)
	})

	t.Run("CL-11 the limit does not increase above MaxLimit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1), WithMaxLimit(2))
		runTimes(t, l, 100, nil)
		assertLimit(t, l, 2)
		holding(l, 2, func() { assertRejected(t, l) })
	})
}

func TestContext(t *testing.T) {
	t.Run("CL-13 TK-13 a context that has already ended returns its error and runs nothing", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		for range 5 {
			err := l.Do(ctx, func(context.Context) error {
				calls++
				return nil
			})
			if err != context.Canceled {
				t.Fatalf("Do() = %v, want context.Canceled", err)
			}
		}
		if calls != 0 || l.InFlight() != 0 {
			t.Fatalf("calls = %d, InFlight() = %d, want 0, 0", calls, l.InFlight())
		}
		assertLimit(t, l, 1)
	})

	t.Run("CL-13 a context that has already ended returns its error when the limiter is full", func(t *testing.T) {
		l, _ := newLimiter(t, fixedLimit(1)...)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		holding(l, 1, func() {
			if err := l.Do(ctx, succeed); err != context.Canceled {
				t.Errorf("Do() = %v, want context.Canceled", err)
			}
		})
	})

	t.Run("CL-14 the call gets the context of the caller", func(t *testing.T) {
		type key struct{}
		l, _ := newLimiter(t)
		var got any
		_ = l.Do(context.WithValue(context.Background(), key{}, "value"), func(ctx context.Context) error {
			got = ctx.Value(key{})
			return nil
		})
		if got != "value" {
			t.Fatalf("value = %v", got)
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("CL-15 TK-17 a panic releases the call and continues with the same value", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1), WithOverloadIf(func(error) bool { return true }))
		recovered := func() (value any) {
			defer func() { value = recover() }()
			_ = l.Do(context.Background(), func(context.Context) error { panic("broken") })
			return nil
		}()
		if recovered != "broken" {
			t.Fatalf("recovered = %v, want broken", recovered)
		}
		if l.InFlight() != 0 {
			t.Fatalf("InFlight() = %d, want 0", l.InFlight())
		}
		assertLimit(t, l, 1)
	})

	t.Run("CL-15 a panic does not change the limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1))
		func() {
			defer func() { _ = recover() }()
			_ = l.Do(context.Background(), func(context.Context) error { panic("broken") })
		}()
		assertLimit(t, l, 1)
		runTimes(t, l, 1, nil)
		assertLimit(t, l, 2)
	})
}

func TestErrors(t *testing.T) {
	t.Run("CL-16 TK-11 ErrLimitExceeded matches the common rejection error", func(t *testing.T) {
		if !errors.Is(ErrLimitExceeded, faultvane.ErrRejected) {
			t.Fatal("ErrLimitExceeded does not match faultvane.ErrRejected")
		}
		if want := "concurrencylimit: limit of concurrent calls is exceeded: faultvane: call rejected"; ErrLimitExceeded.Error() != want {
			t.Fatalf("ErrLimitExceeded = %q, want %q", ErrLimitExceeded, want)
		}
	})
}

type sequence struct {
	minLimit, initialLimit, maxLimit int
	ratio                            float64
	events                           []byte
}

func newSequence(minLimit, initialSpan, maxSpan, ratio uint8, events []byte) sequence {
	s := sequence{minLimit: 1 + int(minLimit), ratio: float64(1+ratio%99) / 100, events: events}
	s.initialLimit = s.minLimit + int(initialSpan)
	s.maxLimit = s.initialLimit + int(maxSpan)
	return s
}

func checkInvariants(t *testing.T, s sequence) {
	t.Helper()
	l, clock := newLimiter(t,
		WithMinLimit(s.minLimit), WithInitialLimit(s.initialLimit), WithMaxLimit(s.maxLimit),
		WithBackoffRatio(s.ratio), WithLatencyThreshold(time.Second))
	check := func(step int) {
		if limit := l.Limit(); limit < s.minLimit || limit > s.maxLimit {
			t.Fatalf("CL-8 CL-11 step %d: Limit() = %d, want %d to %d", step, limit, s.minLimit, s.maxLimit)
		}
	}
	for step, event := range s.events {
		var result error
		if event&4 != 0 {
			result = context.DeadlineExceeded
		}
		holding(l, int(event&3), func() {
			inFlight, limit, ran := l.InFlight(), l.Limit(), false
			err := l.Do(context.Background(), func(context.Context) error {
				ran = true
				if event&8 != 0 {
					clock.Advance(time.Second)
				}
				return result
			})
			wantRejection := inFlight >= limit
			if wantRejection != (err == ErrLimitExceeded) || wantRejection == ran || (ran && err != result) {
				t.Fatalf("CL-2 step %d: Do() = %v, ran = %v, with %d calls in progress and limit %d", step, err, ran, inFlight, limit)
			}
			overload := event&12 != 0
			if after := l.Limit(); ran && ((overload && after > limit) || (!overload && after < limit)) {
				t.Fatalf("CL-4 CL-9 step %d: the limit went from %d to %d, overload signal = %v", step, limit, after, overload)
			}
			check(step)
		})
		check(step)
		if l.InFlight() != 0 {
			t.Fatalf("CL-18 step %d: InFlight() = %d, want 0", step, l.InFlight())
		}
	}
}

func FuzzLimit(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), []byte{})
	f.Add(uint8(0), uint8(0), uint8(3), uint8(49), []byte{0, 0, 0, 0, 4, 8, 1, 2, 3, 7, 11})
	f.Add(uint8(1), uint8(2), uint8(2), uint8(89), []byte{1, 1, 1, 1, 1, 2, 2, 2, 5, 9, 12, 3, 3})
	f.Fuzz(func(t *testing.T, minLimit, initialSpan, maxSpan, ratio uint8, events []byte) {
		checkInvariants(t, newSequence(minLimit, initialSpan, maxSpan, ratio, events))
	})
}

func TestLimitInvariants(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		events := make([]byte, random.IntN(60))
		for i := range events {
			events[i] = byte(random.UintN(256))
		}
		checkInvariants(t, newSequence(uint8(random.UintN(3)), uint8(random.UintN(4)), uint8(random.UintN(4)), uint8(random.UintN(256)), events))
	}
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

	raise := func(highest *atomic.Int64, value int64) {
		for {
			seen := highest.Load()
			if value <= seen || highest.CompareAndSwap(seen, value) {
				return
			}
		}
	}

	t.Run("CL-2 CL-17 concurrent callers get exactly the limit", func(t *testing.T) {
		const limit = 5
		l, _ := newLimiter(t, fixedLimit(limit)...)
		var rejected atomic.Int64
		entered := make(chan struct{}, goroutines)
		leave := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			together(func(int) {
				err := l.Do(context.Background(), func(context.Context) error {
					entered <- struct{}{}
					<-leave
					return nil
				})
				if err == ErrLimitExceeded {
					rejected.Add(1)
				}
			})
		}()
		deadline := time.Now().Add(10 * time.Second)
		for rejected.Load() != goroutines-limit || len(entered) != limit {
			if time.Now().After(deadline) {
				t.Fatalf("rejected = %d, calls in progress = %d, want %d, %d", rejected.Load(), len(entered), goroutines-limit, limit)
			}
			runtime.Gosched()
		}
		if l.InFlight() != limit {
			t.Fatalf("InFlight() = %d, want %d", l.InFlight(), limit)
		}
		close(leave)
		<-done
		if rejected.Load() != goroutines-limit || len(entered) != limit || l.InFlight() != 0 {
			t.Fatalf("rejected = %d, ran = %d, InFlight() = %d", rejected.Load(), len(entered), l.InFlight())
		}
	})

	t.Run("CL-17 TK-14 concurrent calls never exceed the limit and keep it in its range", func(t *testing.T) {
		const rounds, minLimit, maxLimit = 500, 2, 8
		l, _ := newLimiter(t, WithMinLimit(minLimit), WithInitialLimit(4), WithMaxLimit(maxLimit), WithBackoffRatio(0.5))
		var active, highest, ran, rejected atomic.Int64
		together(func(id int) {
			for round := range rounds {
				var result error
				if (id+round)%5 == 0 {
					result = context.DeadlineExceeded
				}
				err := l.Do(context.Background(), func(context.Context) error {
					raise(&highest, active.Add(1))
					if limit := l.Limit(); limit < minLimit || limit > maxLimit {
						t.Errorf("Limit() = %d, want %d to %d", limit, minLimit, maxLimit)
					}
					runtime.Gosched()
					active.Add(-1)
					ran.Add(1)
					return result
				})
				switch err {
				case result:
				case ErrLimitExceeded:
					rejected.Add(1)
				default:
					t.Errorf("Do() = %v", err)
				}
			}
		})
		if highest.Load() < 1 || highest.Load() > maxLimit {
			t.Fatalf("highest number of calls in progress = %d, want 1 to %d", highest.Load(), maxLimit)
		}
		if total := ran.Load() + rejected.Load(); total != goroutines*rounds {
			t.Fatalf("ran %d + rejected %d = %d, want %d", ran.Load(), rejected.Load(), total, goroutines*rounds)
		}
		if limit := l.Limit(); l.InFlight() != 0 || limit < minLimit || limit > maxLimit {
			t.Fatalf("InFlight() = %d, Limit() = %d", l.InFlight(), limit)
		}
	})

	t.Run("CL-17 a change of the limit during a different change is not lost", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(100))
		interrupted := false
		l.adapt(func(limit float64) float64 {
			if !interrupted {
				interrupted = true
				l.adapt(func(limit float64) float64 { return limit / 2 })
			}
			return limit - 1
		})
		assertLimit(t, l, 49)
	})

	t.Run("CL-17 concurrent overload signals each decrease the limit one time", func(t *testing.T) {
		const signals = 10
		l, _ := newLimiter(t, WithInitialLimit(1<<20), WithMaxLimit(1<<20), WithBackoffRatio(0.5))
		for range 20 {
			l.limit.Store(math.Float64bits(1 << 20))
			together(func(id int) {
				if id < signals {
					_ = run(l, context.DeadlineExceeded)
				}
			})
			assertLimit(t, l, 1<<20>>signals)
		}
	})
}

func TestResources(t *testing.T) {
	t.Run("CL-22 TK-15 a limiter starts no goroutine", func(t *testing.T) {
		before := runtime.NumGoroutine()
		l, _ := newLimiter(t, WithInitialLimit(1))
		runTimes(t, l, 5, nil)
		runTimes(t, l, 5, context.DeadlineExceeded)
		holding(l, 1, func() { assertRejected(t, l) })
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("CL-22 TK-15 the limiter code contains no go statement", func(t *testing.T) {
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

	t.Run("CL-23 TK-50 a call below the limit makes no allocation", func(t *testing.T) {
		ctx := context.Background()
		overloaded := func(context.Context) error { return context.DeadlineExceeded }
		cases := []struct {
			name string
			opts []Option
			call func(context.Context) error
		}{
			{"default configuration", nil, succeed},
			{"system clock and latency threshold", []Option{WithClock(faultvane.SystemClock()), WithLatencyThreshold(time.Hour)}, succeed},
			{"increase of the limit", []Option{WithInitialLimit(1), WithMaxLimit(1 << 20)}, succeed},
			{"decrease of the limit", []Option{WithInitialLimit(200)}, overloaded},
		}
		for _, c := range cases {
			l, _ := newLimiter(t, c.opts...)
			if allocations := testing.AllocsPerRun(1000, func() { _ = l.Do(ctx, c.call) }); allocations != 0 {
				t.Errorf("%s: %v allocations for each call, want 0", c.name, allocations)
			}
		}
	})

	t.Run("CL-23 TK-50 a rejection makes no allocation", func(t *testing.T) {
		l, _ := newLimiter(t, fixedLimit(1)...)
		ctx := context.Background()
		holding(l, 1, func() {
			if allocations := testing.AllocsPerRun(1000, func() { _ = l.Do(ctx, succeed) }); allocations != 0 {
				t.Errorf("%v allocations for each rejection, want 0", allocations)
			}
		})
	})

	t.Run("CL-1 the limiter works with the system clock", func(t *testing.T) {
		l, err := New(WithInitialLimit(1), WithLatencyThreshold(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := run(l, nil); err != nil || l.Limit() != 2 {
			t.Fatalf("Do() = %v, Limit() = %d, want nil, 2", err, l.Limit())
		}
	})
}
