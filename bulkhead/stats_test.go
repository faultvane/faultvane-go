package bulkhead

import (
	"context"
	"sync"
	"testing"
	"time"
)

func assertStats(t *testing.T, b *Bulkhead, want Stats) {
	t.Helper()
	if got := b.Stats(); got != want {
		t.Fatalf("Stats() = %+v, want %+v", got, want)
	}
}

func TestStats(t *testing.T) {
	t.Run("BH-23 a new bulkhead has zero statistics", func(t *testing.T) {
		assertStats(t, newBulkhead(t), Stats{})
	})

	t.Run("BH-23 Stats counts permitted, waiting, and rejected calls", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		target := newDependency(t, b)
		first := target.hold(1)
		assertStats(t, b, Stats{Permitted: 1, InFlight: 1})
		second := target.wait(context.Background(), 2, 1)
		assertStats(t, b, Stats{Permitted: 1, Waited: 1, InFlight: 1, Waiting: 1})
		if err := b.Do(context.Background(), succeed); err != ErrFull {
			t.Fatalf("Do() = %v, want ErrFull", err)
		}
		assertStats(t, b, Stats{Permitted: 1, Rejected: 1, Waited: 1, InFlight: 1, Waiting: 1})
		target.finish(first)
		target.awaitEntry(2)
		assertStats(t, b, Stats{Permitted: 2, Rejected: 1, Waited: 1, InFlight: 1})
		target.finish(second)
		eventually(t, "the slot is free", func() bool { return b.InFlight() == 0 })
		assertStats(t, b, Stats{Permitted: 2, Rejected: 1, Waited: 1})
	})

	t.Run("BH-23 a waiting caller whose MaxWait ends is a rejected call", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1), WithMaxWait(time.Millisecond))
		target := newDependency(t, b)
		first := target.hold(1)
		if err := receive(t, "the wait ends", target.call(context.Background(), 2)); err != ErrFull {
			t.Fatalf("Do() = %v, want ErrFull", err)
		}
		assertStats(t, b, Stats{Permitted: 1, Rejected: 1, Waited: 1, InFlight: 1})
		target.finish(first)
	})

	t.Run("BH-23 a waiting caller whose context ends is not permitted and not rejected", func(t *testing.T) {
		b := newBulkhead(t, WithMaxConcurrent(1), WithMaxWaiting(1))
		target := newDependency(t, b)
		first := target.hold(1)
		ctx, cancel := context.WithCancel(context.Background())
		second := target.wait(ctx, 2, 1)
		cancel()
		if err := receive(t, "the wait ends", second); err != context.Canceled {
			t.Fatalf("Do() = %v, want context.Canceled", err)
		}
		assertStats(t, b, Stats{Permitted: 1, Waited: 1, InFlight: 1})
		target.finish(first)
	})

	t.Run("BH-23 a context that has already ended changes no count", func(t *testing.T) {
		b := newBulkhead(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = b.Do(ctx, succeed)
		assertStats(t, b, Stats{})
	})

	t.Run("BH-23 TK-14 each concurrent call is permitted or rejected one time", func(t *testing.T) {
		const goroutines, calls = 16, 200
		b := newBulkhead(t, WithMaxConcurrent(2), WithMaxWaiting(2))
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range calls {
					_ = b.Do(context.Background(), succeed)
					_ = b.Stats()
				}
			}()
		}
		group.Wait()
		got := b.Stats()
		if got.Permitted+got.Rejected != goroutines*calls || got.Waited > got.Permitted || got.InFlight != 0 || got.Waiting != 0 {
			t.Fatalf("Stats() = %+v, want %d permitted and rejected calls together and no call in progress", got, goroutines*calls)
		}
	})
}
