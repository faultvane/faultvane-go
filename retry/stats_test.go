package retry

import (
	"context"
	"sync"
	"testing"
	"time"
)

func assertStats(t *testing.T, r *Retry, want Stats) {
	t.Helper()
	if got := r.Stats(); got != want {
		t.Fatalf("Stats() = %+v, want %+v", got, want)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()

	t.Run("RT-21 a new policy has zero statistics", func(t *testing.T) {
		assertStats(t, withoutWaits(t), Stats{})
	})

	t.Run("RT-21 a successful first attempt is a call without a retry", func(t *testing.T) {
		r := withoutWaits(t)
		_ = r.Do(ctx, (&dependency{}).respond(nil))
		assertStats(t, r, Stats{Calls: 1})
	})

	t.Run("RT-21 a successful retry is a retry and not a failed call", func(t *testing.T) {
		r := withoutWaits(t)
		_ = r.Do(ctx, (&dependency{}).respond(errDependency, errDependency, nil))
		assertStats(t, r, Stats{Calls: 1, Retries: 2})
	})

	t.Run("RT-21 a call that fails in all attempts is one failed call", func(t *testing.T) {
		r := withoutWaits(t, WithMaxAttempts(4))
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		assertStats(t, r, Stats{Calls: 1, Retries: 3, Failed: 1})
	})

	t.Run("RT-21 a final error is a failed call without a retry and without a stop", func(t *testing.T) {
		r := withoutWaits(t)
		_ = r.Do(ctx, (&dependency{}).respond(errRejection))
		assertStats(t, r, Stats{Calls: 1, Failed: 1})
	})

	t.Run("RT-21 a closed gate is a stop by the gate", func(t *testing.T) {
		open := true
		r := withoutWaits(t, WithGate(func() bool { return open }))
		_ = r.Do(ctx, (&dependency{}).respond(errDependency, nil))
		open = false
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		assertStats(t, r, Stats{Calls: 2, Retries: 1, Failed: 1, GateStops: 1})
	})

	t.Run("RT-21 an empty budget is a stop by the budget", func(t *testing.T) {
		r := withoutWaits(t, WithMaxAttempts(100), WithBudget(0.001))
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		assertStats(t, r, Stats{Calls: 2, Retries: 10, Failed: 2, BudgetStops: 2})
	})

	t.Run("RT-21 a closed gate has priority over an empty budget", func(t *testing.T) {
		open := true
		r := withoutWaits(t, WithMaxAttempts(100), WithBudget(0.001), WithGate(func() bool { return open }))
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		open = false
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		assertStats(t, r, Stats{Calls: 2, Retries: 10, Failed: 2, GateStops: 1, BudgetStops: 1})
	})

	t.Run("RT-21 a wait that the context ends is a failed call without a retry", func(t *testing.T) {
		r := withoutWaits(t)
		r.wait = func(context.Context, time.Duration) bool { return false }
		_ = r.Do(ctx, (&dependency{}).respond(errDependency))
		assertStats(t, r, Stats{Calls: 1, Failed: 1})
	})

	t.Run("RT-21 TK-14 concurrent calls lose no count", func(t *testing.T) {
		const goroutines, calls = 16, 100
		r := withoutWaits(t)
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range calls {
					_ = r.Do(ctx, (&dependency{}).respond(errDependency))
					_ = r.Stats()
				}
			}()
		}
		group.Wait()
		assertStats(t, r, Stats{Calls: goroutines * calls, Retries: 2 * goroutines * calls, Failed: goroutines * calls})
	})
}
