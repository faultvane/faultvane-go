package bulkhead

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

const patience = 10 * time.Second

var errDependency = errors.New("dependency failed")

func newBulkhead(t *testing.T, opts ...Option) *Bulkhead {
	t.Helper()
	b, err := New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func succeed(context.Context) error { return nil }

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("no result in time: %s", what)
		}
		runtime.Gosched()
	}
}

func receive[T any](t *testing.T, what string, from <-chan T) T {
	t.Helper()
	select {
	case value := <-from:
		return value
	case <-time.After(patience):
		t.Fatalf("no result in time: %s", what)
		panic("not reached")
	}
}

func assertCounts(t *testing.T, b *Bulkhead, inFlight, waiting int) {
	t.Helper()
	if b.InFlight() != inFlight || b.Waiting() != waiting {
		t.Fatalf("InFlight() = %d, Waiting() = %d, want %d, %d", b.InFlight(), b.Waiting(), inFlight, waiting)
	}
}

type dependency struct {
	t       *testing.T
	b       *Bulkhead
	entered chan int
	leave   chan struct{}
}

func newDependency(t *testing.T, b *Bulkhead) *dependency {
	return &dependency{t: t, b: b, entered: make(chan int, 64), leave: make(chan struct{})}
}

func (d *dependency) call(ctx context.Context, id int) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- d.b.Do(ctx, func(context.Context) error {
			d.entered <- id
			<-d.leave
			return nil
		})
	}()
	return result
}

func (d *dependency) hold(id int) <-chan error {
	d.t.Helper()
	result := d.call(context.Background(), id)
	d.awaitEntry(id)
	return result
}

func (d *dependency) wait(ctx context.Context, id, position int) <-chan error {
	d.t.Helper()
	result := d.call(ctx, id)
	eventually(d.t, "the caller waits", func() bool { return d.b.Waiting() == position })
	return result
}

func (d *dependency) awaitEntry(id int) {
	d.t.Helper()
	if got := receive(d.t, "a call enters the dependency", d.entered); got != id {
		d.t.Fatalf("call %d entered the dependency, want call %d", got, id)
	}
}

func (d *dependency) finish(result <-chan error) {
	d.t.Helper()
	d.leave <- struct{}{}
	if err := receive(d.t, "the call returns", result); err != nil {
		d.t.Fatalf("Do() = %v, want nil", err)
	}
}

func (d *dependency) assertIdle() {
	d.t.Helper()
	if len(d.entered) != 0 {
		d.t.Fatalf("%d more calls entered the dependency", len(d.entered))
	}
}

func rejectedWhileFull(b *Bulkhead, ctx context.Context) (err error, calls int) {
	_ = b.Do(context.Background(), func(context.Context) error {
		err = b.Do(ctx, func(context.Context) error {
			calls++
			return nil
		})
		return nil
	})
	return err, calls
}

func TestCalls(t *testing.T) {
	t.Run("BH-1 a call runs when a slot is free", func(t *testing.T) {
		b := newBulkhead(t)
		calls := 0
		err := b.Do(context.Background(), func(context.Context) error {
			calls++
			assertCounts(t, b, 1, 0)
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("Do() = %v with %d calls, want nil with 1 call", err, calls)
		}
		assertCounts(t, b, 0, 0)
	})

	t.Run("BH-11 TK-12 the error of the call returns unchanged and frees the slot", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1))
		for range 3 {
			if err := b.Do(context.Background(), func(context.Context) error { return errDependency }); err != errDependency {
				t.Fatalf("Do() = %v, want %v", err, errDependency)
			}
		}
		assertCounts(t, b, 0, 0)
	})

	t.Run("BH-21 TK-2 TK-3 the bulkhead works in a pipeline", func(t *testing.T) {
		b := newBulkhead(t)
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(b), func(context.Context) (int, error) { return 3, nil })
		if value != 3 || err != nil {
			t.Fatalf("Execute() = %d, %v", value, err)
		}
	})
}

func TestLimit(t *testing.T) {
	t.Run("BH-2 BH-3 a call above the limit is rejected and does not run", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1))
		if err, calls := rejectedWhileFull(b, context.Background()); err != ErrFull || calls != 0 {
			t.Fatalf("Do() = %v with %d calls, want ErrFull with 0 calls", err, calls)
		}
		assertCounts(t, b, 0, 0)
	})

	t.Run("BH-2 BH-16 the bulkhead runs calls up to the limit at the same time", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(3))
		target := newDependency(t, b)
		results := []<-chan error{target.hold(1), target.hold(2), target.hold(3)}
		assertCounts(t, b, 3, 0)
		if err := b.Do(context.Background(), succeed); err != ErrFull {
			t.Fatalf("Do() = %v, want ErrFull", err)
		}
		target.finish(results[0])
		eventually(t, "the slot is free", func() bool { return b.InFlight() == 2 })
		if err := b.Do(context.Background(), succeed); err != nil {
			t.Fatalf("Do() = %v, want nil after a slot became free", err)
		}
		target.finish(results[1])
		target.finish(results[2])
		eventually(t, "all slots are free", func() bool { return b.InFlight() == 0 })
	})
}

func TestWaiting(t *testing.T) {
	t.Run("BH-4 BH-7 BH-16 a waiting caller runs when a slot becomes free", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		target := newDependency(t, b)
		first := target.hold(1)
		second := target.wait(context.Background(), 2, 1)
		assertCounts(t, b, 1, 1)
		target.assertIdle()
		target.finish(first)
		target.awaitEntry(2)
		assertCounts(t, b, 1, 0)
		target.finish(second)
		eventually(t, "the slot is free", func() bool { return b.InFlight() == 0 })
	})

	t.Run("BH-3 BH-22 a caller is rejected when no waiting place is free", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		target := newDependency(t, b)
		first := target.hold(1)
		second := target.wait(context.Background(), 2, 1)
		calls := 0
		err := b.Do(context.Background(), func(context.Context) error {
			calls++
			return nil
		})
		if err != ErrFull || calls != 0 {
			t.Fatalf("Do() = %v with %d calls, want ErrFull with 0 calls", err, calls)
		}
		assertCounts(t, b, 1, 1)
		target.finish(first)
		target.awaitEntry(2)
		target.finish(second)
	})

	t.Run("BH-5 waiting callers get a slot in the order of arrival", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(4))
		target := newDependency(t, b)
		results := []<-chan error{target.hold(0)}
		for id := 1; id <= 4; id++ {
			results = append(results, target.wait(context.Background(), id, id))
		}
		for id := 1; id <= 4; id++ {
			target.finish(results[id-1])
			target.awaitEntry(id)
			assertCounts(t, b, 1, 4-id)
		}
		target.finish(results[4])
	})

	t.Run("BH-6 a waiting caller whose MaxWait ends gets ErrFull", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1), WithMaxWait(time.Millisecond))
		target := newDependency(t, b)
		first := target.hold(1)
		if err := receive(t, "the wait ends", target.call(context.Background(), 2)); err != ErrFull {
			t.Fatalf("Do() = %v, want ErrFull", err)
		}
		assertCounts(t, b, 1, 0)
		target.assertIdle()
		target.finish(first)
	})

	t.Run("BH-6 a waiting caller gets a slot before its MaxWait ends", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1), WithMaxWait(time.Hour))
		target := newDependency(t, b)
		first := target.hold(1)
		second := target.wait(context.Background(), 2, 1)
		target.finish(first)
		target.awaitEntry(2)
		target.finish(second)
	})

	t.Run("BH-8 TK-13 a waiting caller whose context ends gets the context error", func(t *testing.T) {
		for _, opts := range [][]Option{nil, {WithMaxWait(time.Hour)}} {
			b := newBulkhead(t, append([]Option{WithMaxConcurrent(1), WithMaxWaiting(1)}, opts...)...)
			target := newDependency(t, b)
			first := target.hold(1)
			ctx, cancel := context.WithCancel(context.Background())
			second := target.wait(ctx, 2, 1)
			cancel()
			if err := receive(t, "the waiting caller returns", second); err != context.Canceled {
				t.Fatalf("Do() = %v, want context.Canceled", err)
			}
			assertCounts(t, b, 1, 0)
			target.assertIdle()
			target.finish(first)
		}
	})

	t.Run("BH-8 a caller that stops waiting frees its waiting place and gets no slot", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(2))
		target := newDependency(t, b)
		first := target.hold(1)
		ctx, cancel := context.WithCancel(context.Background())
		second := target.wait(ctx, 2, 1)
		third := target.wait(context.Background(), 3, 2)
		cancel()
		if err := receive(t, "the waiting caller returns", second); err != context.Canceled {
			t.Fatalf("Do() = %v, want context.Canceled", err)
		}
		fourth := target.wait(context.Background(), 4, 2)
		target.finish(first)
		target.awaitEntry(3)
		target.finish(third)
		target.awaitEntry(4)
		target.finish(fourth)
		eventually(t, "the slot is free", func() bool { return b.InFlight() == 0 })
	})

	t.Run("BH-20 a caller that gets a slot when its context ends runs the call", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		if err := b.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		target := newDependency(t, b)
		ctx, cancel := context.WithCancel(context.Background())
		waiting := target.wait(ctx, 1, 1)
		b.mu.Lock()
		cancel()
		b.passSlot()
		b.mu.Unlock()
		target.awaitEntry(1)
		assertCounts(t, b, 1, 0)
		target.finish(waiting)
		eventually(t, "the slot is free", func() bool { return b.InFlight() == 0 })
	})
}

func TestContext(t *testing.T) {
	t.Run("BH-9 TK-13 a context that has already ended returns its error and runs nothing", func(t *testing.T) {
		b := newBulkhead(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := b.Do(ctx, func(context.Context) error {
			calls++
			return nil
		})
		if err != context.Canceled || calls != 0 {
			t.Fatalf("Do() = %v with %d calls, want context.Canceled with 0 calls", err, calls)
		}
		assertCounts(t, b, 0, 0)
	})

	t.Run("BH-9 a context that has already ended does not use a waiting place", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err, calls := rejectedWhileFull(b, ctx); err != context.Canceled || calls != 0 {
			t.Fatalf("Do() = %v with %d calls, want context.Canceled with 0 calls", err, calls)
		}
	})

	t.Run("BH-10 the call gets the context of the caller", func(t *testing.T) {
		type key struct{}
		b := newBulkhead(t)
		var got any
		_ = b.Do(context.WithValue(context.Background(), key{}, "value"), func(ctx context.Context) error {
			got = ctx.Value(key{})
			return nil
		})
		if got != "value" {
			t.Fatalf("value = %v", got)
		}
	})
}

func TestPanic(t *testing.T) {
	t.Run("BH-12 TK-17 a panic frees the slot and continues with the same value", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1))
		recovered := func() (value any) {
			defer func() { value = recover() }()
			_ = b.Do(context.Background(), func(context.Context) error { panic("broken") })
			return nil
		}()
		if recovered != "broken" {
			t.Fatalf("recovered = %v, want broken", recovered)
		}
		assertCounts(t, b, 0, 0)
		if err := b.Do(context.Background(), succeed); err != nil {
			t.Fatalf("Do() = %v, want nil after the panic", err)
		}
	})
}

func TestErrors(t *testing.T) {
	t.Run("BH-13 TK-11 ErrFull matches the common rejection error", func(t *testing.T) {
		if !errors.Is(ErrFull, faultvane.ErrRejected) {
			t.Fatal("ErrFull does not match faultvane.ErrRejected")
		}
		if want := "bulkhead: all slots are in use: faultvane: call rejected"; ErrFull.Error() != want {
			t.Fatalf("ErrFull = %q, want %q", ErrFull, want)
		}
	})
}

func TestConcurrency(t *testing.T) {
	const goroutines, rounds, limit, places = 64, 300, 5, 8

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

	cases := []struct {
		name          string
		opts          []Option
		wantRejection bool
	}{
		{"without waiting", nil, true},
		{"with waiting", []Option{WithMaxWaiting(places)}, true},
		{"with a short MaxWait", []Option{WithMaxWaiting(places), WithMaxWait(time.Microsecond)}, true},
		{"with a waiting place for each caller", []Option{WithMaxWaiting(goroutines)}, false},
	}
	for _, c := range cases {
		t.Run("BH-2 BH-17 BH-22 TK-14 the calls in progress never exceed the limit "+c.name, func(t *testing.T) {
			b := newBulkhead(t, append([]Option{WithMaxConcurrent(limit)}, c.opts...)...)
			var active, highest, highestWaiting, ran, rejected, cancelled atomic.Int64
			together(func(id int) {
				for round := range rounds {
					ctx, cancel := context.WithCancel(context.Background())
					if c.wantRejection && (id+round)%7 == 0 {
						cancel()
					}
					err := b.Do(ctx, func(context.Context) error {
						raise(&highest, active.Add(1))
						raise(&highestWaiting, int64(b.Waiting()))
						runtime.Gosched()
						active.Add(-1)
						ran.Add(1)
						return nil
					})
					cancel()
					switch err {
					case nil:
					case ErrFull:
						rejected.Add(1)
					case context.Canceled:
						cancelled.Add(1)
					default:
						t.Errorf("Do() = %v", err)
					}
				}
			})
			if highest.Load() < 1 || highest.Load() > limit {
				t.Fatalf("highest number of calls in progress = %d, want 1 to %d", highest.Load(), limit)
			}
			if highestWaiting.Load() > int64(b.cfg.maxWaiting) {
				t.Fatalf("highest number of waiting callers = %d, want at most %d", highestWaiting.Load(), b.cfg.maxWaiting)
			}
			if total := ran.Load() + rejected.Load() + cancelled.Load(); total != goroutines*rounds {
				t.Fatalf("ran %d + rejected %d + cancelled %d = %d, want %d", ran.Load(), rejected.Load(), cancelled.Load(), total, goroutines*rounds)
			}
			if !c.wantRejection && ran.Load() != goroutines*rounds {
				t.Fatalf("ran = %d, want all %d calls", ran.Load(), goroutines*rounds)
			}
			assertCounts(t, b, 0, 0)
		})
	}
}

func TestResources(t *testing.T) {
	t.Run("BH-18 TK-15 a bulkhead starts no goroutine", func(t *testing.T) {
		before := runtime.NumGoroutine()
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1), WithMaxWait(time.Nanosecond))
		if err, _ := rejectedWhileFull(b, context.Background()); err != ErrFull {
			t.Fatalf("Do() = %v, want ErrFull", err)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("goroutines went from %d to %d", before, after)
		}
	})

	t.Run("BH-18 TK-15 the bulkhead code contains no go statement", func(t *testing.T) {
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

	t.Run("BH-19 TK-50 a call with a free slot makes no allocation", func(t *testing.T) {
		b := newBulkhead(t, WithMaxWaiting(1), WithMaxWait(time.Second))
		ctx := context.Background()
		if allocations := testing.AllocsPerRun(1000, func() { _ = b.Do(ctx, succeed) }); allocations != 0 {
			t.Fatalf("%v allocations for each call, want 0", allocations)
		}
	})

	t.Run("BH-19 TK-50 a rejection makes no allocation", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1))
		ctx := context.Background()
		_ = b.Do(ctx, func(context.Context) error {
			if allocations := testing.AllocsPerRun(1000, func() { _ = b.Do(ctx, succeed) }); allocations != 0 {
				t.Errorf("%v allocations for each rejection, want 0", allocations)
			}
			return nil
		})
	})
}
