package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/faultvanetest"
)

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	acquire := func(t *testing.T, store Store, request Acquire) Grant {
		t.Helper()
		grant, err := store.Acquire(ctx, "a", request)
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}

	t.Run("RL-21 the bucket starts full and refills by the clock of the store", func(t *testing.T) {
		clock := faultvanetest.NewClock(testStart)
		store := NewMemoryStore(clock)
		steps := []struct {
			advance time.Duration
			request Acquire
			want    int64
		}{
			{0, Acquire{Wanted: 3, Rate: 10, Burst: 5}, 3},
			{0, Acquire{Wanted: 3, Rate: 10, Burst: 5}, 2},
			{0, Acquire{Wanted: 1, Rate: 10, Burst: 5}, 0},
			{99 * time.Millisecond, Acquire{Wanted: 1, Rate: 10, Burst: 5}, 0},
			{time.Millisecond, Acquire{Wanted: 2, Rate: 10, Burst: 5}, 1},
			{250 * time.Millisecond, Acquire{Wanted: 0, Rate: 10, Burst: 5}, 0},
			{50 * time.Millisecond, Acquire{Wanted: 9, Rate: 10, Burst: 5}, 3},
			{time.Hour, Acquire{Wanted: 9, Rate: 10, Burst: 5}, 5},
			{0, Acquire{Wanted: 9, Returned: 2, Rate: 10, Burst: 5}, 2},
			{0, Acquire{Wanted: 0, Returned: 7, Rate: 10, Burst: 5}, 0},
			{0, Acquire{Wanted: 9, Rate: 10, Burst: 5}, 5},
			{time.Hour, Acquire{Wanted: -1, Rate: 10, Burst: 5}, 0},
			{0, Acquire{Wanted: 9, Rate: 10, Burst: 5}, 5},
		}
		for i, step := range steps {
			clock.Advance(step.advance)
			grant := acquire(t, store, step.request)
			if grant.Tokens != step.want || !grant.StoreTime.Equal(clock.Now()) {
				t.Fatalf("step %d: grant = %+v, want %d tokens at %v", i, grant, step.want, clock.Now())
			}
		}
	})

	t.Run("RL-21 a clock that goes back gives no tokens and no earlier store time", func(t *testing.T) {
		clock := faultvanetest.NewClock(testStart)
		store := NewMemoryStore(clock)
		request := Acquire{Wanted: 5, Rate: 10, Burst: 5}
		acquire(t, store, request)
		clock.Advance(-time.Second)
		if grant := acquire(t, store, request); grant.Tokens != 0 || !grant.StoreTime.Equal(testStart) {
			t.Fatalf("grant after the clock went back = %+v, want no tokens at %v", grant, testStart)
		}
		clock.Advance(time.Second + 100*time.Millisecond)
		if grant := acquire(t, store, request); grant.Tokens != 1 {
			t.Fatalf("grant 100 ms after the last store time = %+v, want 1 token", grant)
		}
	})

	t.Run("RL-21 an ended context returns its error and changes nothing", func(t *testing.T) {
		store := NewMemoryStore(faultvanetest.NewClock(testStart))
		ended, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Acquire(ended, "a", Acquire{Wanted: 5, Rate: 10, Burst: 5}); err != context.Canceled {
			t.Fatalf("Acquire() error = %v, want context.Canceled", err)
		}
		if grant := acquire(t, store, Acquire{Wanted: 5, Rate: 10, Burst: 5}); grant.Tokens != 5 {
			t.Fatalf("grant = %+v, want 5 tokens", grant)
		}
	})
}
