package loadshed

import (
	"context"
	"sync"
	"testing"
)

func assertStats(t *testing.T, s *Shedder, want Stats) {
	t.Helper()
	if got := s.Stats(); got != want {
		t.Fatalf("Stats() = %+v, want %+v", got, want)
	}
}

func TestStats(t *testing.T) {
	t.Run("LS-21 a new shedder has zero statistics", func(t *testing.T) {
		assertStats(t, newTestShedder(t), Stats{})
	})

	t.Run("LS-21 Stats counts the admitted calls of each priority", func(t *testing.T) {
		shedder := newTestShedder(t)
		for p, calls := range map[Priority]int{Critical: 1, High: 2, Normal: 3, Low: 4} {
			for range calls {
				_ = shedder.Do(with(p), succeed)
			}
		}
		_ = shedder.Do(context.Background(), succeed)
		assertStats(t, shedder, Stats{Admitted: [priorities]int64{Critical: 1, High: 2, Normal: 4, Low: 4}})
	})

	t.Run("LS-21 Stats counts the shed calls of each priority and the calls in progress", func(t *testing.T) {
		shedder := newTestShedder(t, WithMaxInFlight(4))
		whileInFlight(t, shedder, 3, func() {
			_ = shedder.Do(with(Low), succeed)
			_ = shedder.Do(with(Low), succeed)
			_ = shedder.Do(with(Normal), succeed)
			_ = shedder.Do(with(Critical), succeed)
			assertStats(t, shedder, Stats{
				Admitted: [priorities]int64{Critical: 4},
				Shed:     [priorities]int64{Normal: 1, Low: 2},
				InFlight: 3,
			})
		})
		whileInFlight(t, shedder, 4, func() {
			_ = shedder.Do(with(Critical), succeed)
			_ = shedder.Do(with(High), succeed)
		})
		assertStats(t, shedder, Stats{
			Admitted: [priorities]int64{Critical: 8},
			Shed:     [priorities]int64{Critical: 1, High: 1, Normal: 1, Low: 2},
		})
	})

	t.Run("LS-21 a context that has already ended changes no count", func(t *testing.T) {
		shedder := newTestShedder(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = shedder.Do(ctx, succeed)
		assertStats(t, shedder, Stats{})
	})

	t.Run("LS-21 TK-14 each concurrent call is admitted or shed one time", func(t *testing.T) {
		const goroutines, calls = 16, 200
		shedder := newTestShedder(t, WithMaxInFlight(2))
		var group sync.WaitGroup
		for range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				for i := range calls {
					_ = shedder.Do(with(levels[i%len(levels)]), succeed)
					_ = shedder.Stats()
				}
			}()
		}
		group.Wait()
		got := shedder.Stats()
		for _, p := range levels {
			if got.Admitted[p]+got.Shed[p] != goroutines*calls/priorities {
				t.Errorf("priority %d: %d admitted and %d shed, want %d together", p, got.Admitted[p], got.Shed[p], goroutines*calls/priorities)
			}
		}
		if got.InFlight != 0 {
			t.Errorf("InFlight = %d, want 0", got.InFlight)
		}
	})
}
