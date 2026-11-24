package faultvanetest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/faultvanetest"
)

var _ faultvane.Clock = (*faultvanetest.Clock)(nil)

func TestClock(t *testing.T) {
	start := time.Unix(1000, 0)

	t.Run("Now returns the start time", func(t *testing.T) {
		clock := faultvanetest.NewClock(start)
		if got := clock.Now(); !got.Equal(start) {
			t.Fatalf("Now() = %v, want %v", got, start)
		}
	})

	t.Run("Advance moves the time", func(t *testing.T) {
		clock := faultvanetest.NewClock(start)
		clock.Advance(90 * time.Second)
		clock.Advance(time.Millisecond)
		if got, want := clock.Now(), start.Add(90*time.Second+time.Millisecond); !got.Equal(want) {
			t.Fatalf("Now() = %v, want %v", got, want)
		}
	})

	t.Run("concurrent use is safe", func(t *testing.T) {
		clock := faultvanetest.NewClock(start)
		var group sync.WaitGroup
		for range 50 {
			group.Add(1)
			go func() {
				defer group.Done()
				clock.Advance(time.Second)
				_ = clock.Now()
			}()
		}
		group.Wait()
		if got, want := clock.Now(), start.Add(50*time.Second); !got.Equal(want) {
			t.Fatalf("Now() = %v, want %v", got, want)
		}
	})
}
