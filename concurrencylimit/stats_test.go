package concurrencylimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func assertStats(t *testing.T, l *Limiter, want Stats) {
	t.Helper()
	if got := l.Stats(); got != want {
		t.Fatalf("Stats() = %+v, want %+v", got, want)
	}
}

func TestStats(t *testing.T) {
	t.Run("CL-24 a new limiter has no counts and its initial limit", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(7))
		assertStats(t, l, Stats{Limit: 7})
	})

	t.Run("CL-24 Stats counts permitted and rejected calls and the calls in progress", func(t *testing.T) {
		l, _ := newLimiter(t, fixedLimit(2)...)
		holding(l, 2, func() {
			assertRejected(t, l)
			assertRejected(t, l)
			assertStats(t, l, Stats{Permitted: 2, Rejected: 2, Limit: 2, InFlight: 2})
		})
		runTimes(t, l, 1, errDependency)
		assertStats(t, l, Stats{Permitted: 3, Rejected: 2, Limit: 2})
	})

	t.Run("CL-24 each overload signal is one decrease", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5))
		runTimes(t, l, 2, context.DeadlineExceeded)
		runTimes(t, l, 1, nil)
		assertStats(t, l, Stats{Permitted: 3, Decreases: 2, Limit: 2})
	})

	t.Run("CL-24 a slow call is one decrease", func(t *testing.T) {
		l, clock := newLimiter(t, WithInitialLimit(10), WithBackoffRatio(0.5), WithLatencyThreshold(time.Second))
		_ = l.Do(context.Background(), func(context.Context) error {
			clock.Advance(time.Second)
			return nil
		})
		assertStats(t, l, Stats{Permitted: 1, Decreases: 1, Limit: 5})
	})

	t.Run("CL-24 an increase of the limit is not a decrease", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(1), WithMaxLimit(2))
		runTimes(t, l, 1, nil)
		assertStats(t, l, Stats{Permitted: 1, Limit: 2})
	})

	t.Run("CL-24 a context that has already ended changes no count", func(t *testing.T) {
		l, _ := newLimiter(t, WithInitialLimit(7))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = l.Do(ctx, succeed)
		assertStats(t, l, Stats{Limit: 7})
	})

	t.Run("CL-24 TK-14 each concurrent call is permitted or rejected one time", func(t *testing.T) {
		const goroutines, calls = 16, 200
		l, _ := newLimiter(t, fixedLimit(2)...)
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range calls {
					_ = l.Do(context.Background(), succeed)
					_ = l.Stats()
				}
			}()
		}
		group.Wait()
		got := l.Stats()
		if got.Permitted+got.Rejected != goroutines*calls || got.Permitted == 0 || got.Decreases != 0 || got.InFlight != 0 {
			t.Fatalf("Stats() = %+v, want %d permitted and rejected calls together and no call in progress", got, goroutines*calls)
		}
	})
}
