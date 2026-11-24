package loadshed

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

	"github.com/faultvane/faultvane-go"
)

var (
	errDependency = errors.New("dependency failed")
	levels        = []Priority{Critical, High, Normal, Low}
)

func newTestShedder(t testing.TB, opts ...Option) *Shedder {
	t.Helper()
	shedder, err := New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return shedder
}

func succeed(context.Context) error { return nil }

func with(p Priority) context.Context {
	return WithPriority(context.Background(), p)
}

func whileInFlight(t testing.TB, s *Shedder, calls int, work func()) {
	t.Helper()
	if calls == 0 {
		work()
		return
	}
	err := s.Do(with(Critical), func(context.Context) error {
		whileInFlight(t, s, calls-1, work)
		return nil
	})
	if err != nil {
		t.Fatalf("Do() = %v with %d calls still to start", err, calls)
	}
}

func assertDecision(t *testing.T, s *Shedder, inFlight int, ctx context.Context, want error) {
	t.Helper()
	whileInFlight(t, s, inFlight, func() {
		calls := 0
		err := s.Do(ctx, func(context.Context) error {
			calls++
			return nil
		})
		wantCalls := 1
		if want != nil {
			wantCalls = 0
		}
		if err != want || calls != wantCalls {
			t.Fatalf("%d in progress: Do() = %v with %d calls, want %v with %d calls", inFlight, err, calls, want, wantCalls)
		}
	})
}

func TestPriority(t *testing.T) {
	t.Run("LS-3 a context carries each priority", func(t *testing.T) {
		for _, p := range levels {
			if got := PriorityFrom(with(p)); got != p {
				t.Errorf("PriorityFrom() = %d, want %d", got, p)
			}
		}
		if Critical == High || High == Normal || Normal == Low {
			t.Error("two priorities are equal")
		}
	})

	t.Run("LS-3 the innermost priority applies", func(t *testing.T) {
		if got := PriorityFrom(WithPriority(with(Low), Critical)); got != Critical {
			t.Fatalf("PriorityFrom() = %d, want Critical", got)
		}
	})

	t.Run("LS-3 a derived context keeps the priority", func(t *testing.T) {
		ctx, cancel := context.WithCancel(with(High))
		defer cancel()
		if got := PriorityFrom(ctx); got != High {
			t.Fatalf("PriorityFrom() = %d, want High", got)
		}
	})

	t.Run("LS-4 a context without a priority is Normal", func(t *testing.T) {
		if got := PriorityFrom(context.Background()); got != Normal {
			t.Fatalf("PriorityFrom() = %d, want Normal", got)
		}
	})

	t.Run("LS-4 a value that is not a priority level is Normal", func(t *testing.T) {
		for _, p := range []Priority{Critical - 1, Low + 1} {
			if got := PriorityFrom(with(p)); got != Normal {
				t.Errorf("PriorityFrom() = %d for the value %d, want Normal", got, p)
			}
		}
	})
}

func TestDo(t *testing.T) {
	t.Run("LS-1 TK-2 an admitted call runs one time", func(t *testing.T) {
		shedder := newTestShedder(t)
		calls := 0
		err := faultvane.Policy(shedder).Do(context.Background(), func(context.Context) error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, calls)
		}
	})

	t.Run("LS-1 TK-12 the error of the call returns unchanged", func(t *testing.T) {
		shedder := newTestShedder(t)
		if err := shedder.Do(context.Background(), func(context.Context) error { return errDependency }); err != errDependency {
			t.Fatalf("Do() = %v, want %v", err, errDependency)
		}
	})

	t.Run("LS-1 the call receives the context of the caller", func(t *testing.T) {
		shedder := newTestShedder(t)
		ctx := with(High)
		_ = shedder.Do(ctx, func(received context.Context) error {
			if received != ctx {
				t.Error("the call received a different context")
			}
			return nil
		})
	})

	t.Run("LS-1 the shedder works in a pipeline", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(1))
		pipeline := faultvane.Combine(shedder)
		value, err := faultvane.Execute(context.Background(), pipeline, func(ctx context.Context) (int, error) {
			_, inner := faultvane.Execute(ctx, pipeline, func(context.Context) (int, error) { return 1, nil })
			if !errors.Is(inner, faultvane.ErrRejected) {
				t.Errorf("inner Execute() error = %v, want a rejection", inner)
			}
			return 7, nil
		})
		if value != 7 || err != nil {
			t.Fatalf("Execute() = %d, %v, want 7, nil", value, err)
		}
	})

	t.Run("LS-2 LS-10 InFlight counts the calls in progress", func(t *testing.T) {
		shedder := newTestShedder(t)
		if got := shedder.InFlight(); got != 0 {
			t.Fatalf("InFlight() = %d before a call, want 0", got)
		}
		whileInFlight(t, shedder, 3, func() {
			if got := shedder.InFlight(); got != 3 {
				t.Fatalf("InFlight() = %d, want 3", got)
			}
		})
		if got := shedder.InFlight(); got != 0 {
			t.Fatalf("InFlight() = %d after the calls, want 0", got)
		}
	})

	t.Run("LS-10 a call that returns an error is not in progress after it", func(t *testing.T) {
		shedder := newTestShedder(t)
		_ = shedder.Do(context.Background(), func(context.Context) error { return errDependency })
		if got := shedder.InFlight(); got != 0 {
			t.Fatalf("InFlight() = %d, want 0", got)
		}
	})

	t.Run("LS-6 a rejected call does not run and is not in progress", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(2))
		whileInFlight(t, shedder, 2, func() {
			calls := 0
			err := shedder.Do(context.Background(), func(context.Context) error {
				calls++
				return nil
			})
			if err != ErrShed || calls != 0 || shedder.InFlight() != 2 {
				t.Fatalf("Do() = %v with %d calls and %d in progress, want ErrShed, 0, 2", err, calls, shedder.InFlight())
			}
		})
	})

	t.Run("LS-7 TK-11 the rejection matches the common rejection error", func(t *testing.T) {
		if !errors.Is(ErrShed, faultvane.ErrRejected) {
			t.Fatal("ErrShed does not match faultvane.ErrRejected")
		}
		if !strings.HasPrefix(ErrShed.Error(), "loadshed: ") {
			t.Fatalf("message = %q", ErrShed)
		}
	})
}

func TestThresholds(t *testing.T) {
	t.Run("LS-5 LS-11 each priority is rejected at its default threshold and not below it", func(t *testing.T) {
		shedder := newTestShedder(t)
		for p, limit := range map[Priority]int{Low: 50, Normal: 75, High: 90, Critical: 100} {
			assertDecision(t, shedder, limit-1, with(p), nil)
			assertDecision(t, shedder, limit, with(p), ErrShed)
		}
	})

	t.Run("LS-5 a priority stays rejected above its threshold", func(t *testing.T) {
		shedder := newTestShedder(t)
		assertDecision(t, shedder, 51, with(Low), ErrShed)
		assertDecision(t, shedder, 100, with(Low), ErrShed)
		assertDecision(t, shedder, 100, with(High), ErrShed)
	})

	t.Run("LS-4 LS-5 a call without a priority has the Normal threshold", func(t *testing.T) {
		shedder := newTestShedder(t)
		assertDecision(t, shedder, 74, context.Background(), nil)
		assertDecision(t, shedder, 75, context.Background(), ErrShed)
	})

	t.Run("LS-2 the utilization is relative to the maximum", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(10))
		for p, limit := range map[Priority]int{Low: 5, Normal: 8, High: 9, Critical: 10} {
			assertDecision(t, shedder, limit-1, with(p), nil)
			assertDecision(t, shedder, limit, with(p), ErrShed)
		}
	})

	t.Run("LS-2 a maximum of 1 admits each priority when no call is in progress", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(1))
		for _, p := range levels {
			assertDecision(t, shedder, 0, with(p), nil)
			assertDecision(t, shedder, 1, with(p), ErrShed)
		}
	})

	t.Run("LS-13 a configured threshold applies", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(10), WithThreshold(Low, 0.7), WithThreshold(Normal, 0.7), WithThreshold(High, 0.8), WithThreshold(Critical, 0.9))
		for p, limit := range map[Priority]int{Low: 7, Normal: 7, High: 8, Critical: 9} {
			assertDecision(t, shedder, limit-1, with(p), nil)
			assertDecision(t, shedder, limit, with(p), ErrShed)
		}
	})

	t.Run("LS-5 a threshold that is a fraction of the maximum is exact for each maximum", func(t *testing.T) {
		for maximum := 10; maximum <= 300; maximum += 10 {
			shedder := newTestShedder(t, WithMaxInFlight(maximum))
			limit := maximum * 9 / 10
			assertDecision(t, shedder, limit-1, with(High), nil)
			assertDecision(t, shedder, limit, with(High), ErrShed)
		}
	})

	t.Run("LS-8 Critical work is rejected only at the maximum", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(4))
		for inFlight := range 4 {
			assertDecision(t, shedder, inFlight, with(Critical), nil)
		}
		assertDecision(t, shedder, 4, with(Critical), ErrShed)
	})

	t.Run("LS-9 the calls in progress never exceed the maximum", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(3), WithThreshold(Low, 1), WithThreshold(Normal, 1), WithThreshold(High, 1))
		whileInFlight(t, shedder, 3, func() {
			for _, p := range levels {
				if err := shedder.Do(with(p), succeed); err != ErrShed {
					t.Errorf("priority %d: Do() = %v, want ErrShed", p, err)
				}
			}
			if got := shedder.InFlight(); got != 3 {
				t.Fatalf("InFlight() = %d, want 3", got)
			}
		})
	})
}

func TestContext(t *testing.T) {
	t.Run("LS-16 TK-13 a context that has ended returns its error and runs nothing", func(t *testing.T) {
		shedder := newTestShedder(t)
		canceled, cancel := context.WithCancel(with(Critical))
		cancel()
		expired, release := context.WithTimeout(context.Background(), 0)
		defer release()
		for ctx, want := range map[context.Context]error{canceled: context.Canceled, expired: context.DeadlineExceeded} {
			calls := 0
			err := shedder.Do(ctx, func(context.Context) error {
				calls++
				return nil
			})
			if err != want || calls != 0 || shedder.InFlight() != 0 {
				t.Fatalf("Do() = %v with %d calls and %d in progress, want %v, 0, 0", err, calls, shedder.InFlight(), want)
			}
		}
	})

	t.Run("LS-16 a context that has ended has priority over a rejection", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(1))
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		whileInFlight(t, shedder, 1, func() {
			if err := shedder.Do(canceled, succeed); err != context.Canceled {
				t.Fatalf("Do() = %v, want context.Canceled", err)
			}
		})
	})

	t.Run("LS-16 a context that ends during the call does not change the result", func(t *testing.T) {
		shedder := newTestShedder(t)
		ctx, cancel := context.WithCancel(context.Background())
		err := shedder.Do(ctx, func(context.Context) error {
			cancel()
			return errDependency
		})
		if err != errDependency || shedder.InFlight() != 0 {
			t.Fatalf("Do() = %v with %d in progress, want %v, 0", err, shedder.InFlight(), errDependency)
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("LS-17 TK-17 a panic of the call releases its place and continues with the same value", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(1))
		recovered := func() (value any) {
			defer func() { value = recover() }()
			_ = shedder.Do(context.Background(), func(context.Context) error { panic("broken") })
			return nil
		}()
		if recovered != "broken" {
			t.Fatalf("recovered = %v, want broken", recovered)
		}
		if got := shedder.InFlight(); got != 0 {
			t.Fatalf("InFlight() = %d after the panic, want 0", got)
		}
		assertDecision(t, shedder, 0, with(Low), nil)
	})
}

func TestConcurrency(t *testing.T) {
	const goroutines = 64

	together := func(work func()) *sync.WaitGroup {
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
		return &group
	}

	t.Run("LS-9 LS-18 TK-14 concurrent calls that stay in progress fill exactly the maximum", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(10))
		decided := make(chan error, goroutines)
		finish := make(chan struct{})
		group := together(func() {
			err := shedder.Do(with(Critical), func(context.Context) error {
				decided <- nil
				<-finish
				return nil
			})
			if err != nil {
				decided <- err
			}
		})
		admitted := 0
		for range goroutines {
			if err := <-decided; err == nil {
				admitted++
			} else if err != ErrShed {
				t.Errorf("Do() = %v, want ErrShed", err)
			}
		}
		if admitted != 10 || shedder.InFlight() != 10 {
			t.Errorf("admitted = %d, InFlight() = %d, want 10, 10", admitted, shedder.InFlight())
		}
		close(finish)
		group.Wait()
		if got := shedder.InFlight(); got != 0 {
			t.Fatalf("InFlight() = %d after the calls, want 0", got)
		}
	})

	t.Run("LS-9 LS-18 heavy contention never exceeds the maximum", func(t *testing.T) {
		const maximum = 4
		shedder := newTestShedder(t, WithMaxInFlight(maximum))
		var running, peak, admitted, shed atomic.Int64
		contexts := []context.Context{with(Critical), with(High), with(Normal), with(Low), context.Background()}
		together(func() {
			for i := range 2000 {
				err := shedder.Do(contexts[i%len(contexts)], func(context.Context) error {
					now := running.Add(1)
					for seen := peak.Load(); now > seen && !peak.CompareAndSwap(seen, now); seen = peak.Load() {
					}
					runtime.Gosched()
					running.Add(-1)
					return nil
				})
				switch err {
				case nil:
					admitted.Add(1)
				case ErrShed:
					shed.Add(1)
				}
				if inFlight := shedder.InFlight(); inFlight < 0 || inFlight > maximum {
					t.Errorf("InFlight() = %d, want from 0 to %d", inFlight, maximum)
				}
			}
		}).Wait()
		if peak.Load() > maximum || peak.Load() < 1 {
			t.Errorf("peak of calls in progress = %d, want from 1 to %d", peak.Load(), maximum)
		}
		if admitted.Load()+shed.Load() != goroutines*2000 || shedder.InFlight() != 0 {
			t.Errorf("admitted = %d, shed = %d, InFlight() = %d", admitted.Load(), shed.Load(), shedder.InFlight())
		}
	})
}

func TestResources(t *testing.T) {
	t.Run("LS-19 TK-15 a shedder starts no goroutine", func(t *testing.T) {
		before := runtime.NumGoroutine()
		shedder := newTestShedder(t)
		assertDecision(t, shedder, 100, with(Critical), ErrShed)
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("LS-19 TK-15 the shedder code contains no go statement", func(t *testing.T) {
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

	t.Run("LS-20 TK-50 an admitted call makes no allocation", func(t *testing.T) {
		shedder := newTestShedder(t)
		for name, ctx := range map[string]context.Context{"no priority": context.Background(), "priority": with(High)} {
			if allocations := testing.AllocsPerRun(1000, func() { _ = shedder.Do(ctx, succeed) }); allocations != 0 {
				t.Errorf("Do with %s: %v allocations for each call, want 0", name, allocations)
			}
			if allocations := testing.AllocsPerRun(1000, func() { _ = PriorityFrom(ctx) }); allocations != 0 {
				t.Errorf("PriorityFrom with %s: %v allocations for each call, want 0", name, allocations)
			}
		}
	})

	t.Run("LS-20 TK-50 a rejection makes no allocation", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(1))
		ctx := context.Background()
		whileInFlight(t, shedder, 1, func() {
			if allocations := testing.AllocsPerRun(1000, func() { _ = shedder.Do(ctx, succeed) }); allocations != 0 {
				t.Errorf("%v allocations for each call, want 0", allocations)
			}
		})
	})
}

func FuzzShedder(f *testing.F) {
	f.Add(100, 0.5, 0.75, 0.9, 1.0, []byte{0, 1, 2, 3, 4, 0, 0, 0})
	f.Add(1, 0.1, 0.1, 0.1, 0.1, []byte{3, 2, 1, 0, 4, 4})
	f.Add(3, 0.4, 0.7, 0.7, 1.0, []byte{0, 0, 0, 0, 3, 4, 3})
	f.Fuzz(func(t *testing.T, maximum int, low, normal, high, critical float64, steps []byte) {
		shedder, err := New(WithMaxInFlight(maximum), WithThreshold(Low, low), WithThreshold(Normal, normal), WithThreshold(High, high), WithThreshold(Critical, critical))
		if err != nil {
			t.Skip()
		}
		for _, step := range steps {
			before := shedder.InFlight()
			if p := Priority(step % (priorities + 1)); p.valid() {
				admitted := shedder.admit(p)
				if before == 0 && !admitted {
					t.Fatalf("priority %d is rejected with no call in progress", p)
				}
				for more := Critical; admitted && more < p; more++ {
					shedder.inFlight.Add(-1)
					if !shedder.admit(more) {
						t.Fatalf("priority %d is admitted at %d in progress, but priority %d is rejected", p, before, more)
					}
				}
			} else if before > 0 {
				shedder.inFlight.Add(-1)
			}
			if got := shedder.InFlight(); got < 0 || got > maximum {
				t.Fatalf("InFlight() = %d, want from 0 to %d", got, maximum)
			}
		}
	})
}
