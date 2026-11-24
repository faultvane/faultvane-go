package timeout

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	t.Run("TO-16 a new policy has no calls and no timeouts", func(t *testing.T) {
		if got := newTimeout(t, time.Hour).Stats(); got != (Stats{}) {
			t.Fatalf("Stats() = %+v, want zero values", got)
		}
	})

	t.Run("TO-16 a call that ends in time is a call and not a timeout", func(t *testing.T) {
		policy := newTimeout(t, time.Hour)
		_ = policy.Do(context.Background(), func(context.Context) error { return nil })
		_ = policy.Do(context.Background(), func(context.Context) error { return errDependency })
		if got, want := policy.Stats(), (Stats{Calls: 2}); got != want {
			t.Fatalf("Stats() = %+v, want %+v", got, want)
		}
	})

	t.Run("TO-16 a call that the time limit ends is a timeout", func(t *testing.T) {
		policy := newTimeout(t, time.Nanosecond)
		for range 3 {
			_ = policy.Do(context.Background(), untilEnd(errDependency))
		}
		if got, want := policy.Stats(), (Stats{Calls: 3, Timeouts: 3}); got != want {
			t.Fatalf("Stats() = %+v, want %+v", got, want)
		}
	})

	t.Run("TO-16 a call that the caller cancels is not a timeout", func(t *testing.T) {
		policy := newTimeout(t, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = policy.Do(ctx, untilEnd(errDependency))
		if got, want := policy.Stats(), (Stats{Calls: 1}); got != want {
			t.Fatalf("Stats() = %+v, want %+v", got, want)
		}
	})

	t.Run("TO-16 TK-14 concurrent calls lose no count", func(t *testing.T) {
		const goroutines, calls = 16, 50
		policy := newTimeout(t, time.Hour)
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for range calls {
					_ = policy.Do(context.Background(), func(context.Context) error { return nil })
					_ = policy.Stats()
				}
			}()
		}
		group.Wait()
		if got, want := policy.Stats(), (Stats{Calls: goroutines * calls}); got != want {
			t.Fatalf("Stats() = %+v, want %+v", got, want)
		}
	})
}
