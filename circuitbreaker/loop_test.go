package circuitbreaker

import (
	"context"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func newLiveBreaker(t *testing.T, store Store, opts ...Option) *Breaker {
	t.Helper()
	base := []Option{
		WithLogger(quiet), WithStore(store), WithFailureThreshold(5), WithMinimumCalls(1000),
		WithWindowSize(time.Second), WithSyncInterval(20 * time.Millisecond), WithStoreTimeout(10 * time.Millisecond),
		WithEarlyReportInterval(time.Millisecond), WithFaultThreshold(2),
	}
	b, err := New(sharedName, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func liveStore() *testStore {
	return &testStore{Store: NewMemoryStore(faultvane.SystemClock())}
}

func storedState(t *testing.T, store Store) State {
	t.Helper()
	snapshot, err := store.Load(context.Background(), sharedName)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.State
}

func TestSynchronizer(t *testing.T) {
	t.Run("DS-1 the synchronizer brings a transition to the other instances", func(t *testing.T) {
		store := liveStore()
		first, second := newLiveBreaker(t, store), newLiveBreaker(t, store)
		runTimes(t, first, 5, errDependency)
		eventually(t, "both instances are open", func() bool { return first.State() == Open && second.State() == Open })
	})

	t.Run("FD-6 a failure causes a report before the sync interval ends", func(t *testing.T) {
		store := liveStore()
		slow := []Option{WithWindowSize(10 * time.Minute), WithSyncInterval(time.Minute)}
		first, second := newLiveBreaker(t, store, slow...), newLiveBreaker(t, store, slow...)
		runTimes(t, first, 3, errDependency)
		runTimes(t, second, 2, errDependency)
		eventually(t, "the store is open", func() bool { return storedState(t, store.Store) == Open })
	})

	t.Run("FD-7 early reports keep the early report interval", func(t *testing.T) {
		store := liveStore()
		b := newLiveBreaker(t, store, WithWindowSize(10*time.Minute), WithSyncInterval(time.Minute), WithEarlyReportInterval(time.Minute), WithFailureThreshold(1000))
		before := store.operations.Load()
		runTimes(t, b, 1, errDependency)
		eventually(t, "the synchronizer took the signal", func() bool { return len(b.wake) == 0 })
		runTimes(t, b, 50, errDependency)
		eventually(t, "the synchronizer waits", func() bool { return len(b.wake) == 1 })
		if got := store.operations.Load(); got != before {
			t.Fatalf("store operations went from %d to %d inside the early report interval", before, got)
		}
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("DG-1 RC-1 the synchronizer changes to local mode and back", func(t *testing.T) {
		store := liveStore()
		b := newLiveBreaker(t, store)
		store.down.Store(true)
		eventually(t, "local mode", func() bool { return b.Mode() == Local })
		runTimes(t, b, 5, errDependency)
		eventually(t, "the local circuit is open", func() bool { return b.State() == Open })
		store.down.Store(false)
		eventually(t, "distributed mode", func() bool { return b.Mode() == Distributed })
		eventually(t, "the shared circuit is open", func() bool { return storedState(t, store.Store) == Open && b.State() == Open })
	})

	t.Run("RF-3 the synchronizer waits longer between health checks", func(t *testing.T) {
		store := liveStore()
		store.down.Store(true)
		b := newLiveBreaker(t, store)
		eventually(t, "some health checks", func() bool { return store.operations.Load() >= 5 })
		if b.Mode() != Local {
			t.Fatalf("Mode() = %v, want local", b.Mode())
		}
	})
}

func TestClose(t *testing.T) {
	t.Run("API-4 TK-15 Close stops the synchronizer and sends the last report", func(t *testing.T) {
		store := liveStore()
		b := newLiveBreaker(t, store, WithWindowSize(10*time.Minute), WithSyncInterval(time.Minute))
		runTimes(t, b, 7, nil)
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-b.done:
		default:
			t.Fatal("the synchronizer runs after Close")
		}
		record := store.Store.(*MemoryStore).records[sharedName]
		if len(record.Buckets) != 1 || record.Buckets[0].Calls != 7 {
			t.Fatalf("shared window = %+v, want 7 calls", record.Buckets)
		}
		if err := b.Close(context.Background()); err != nil {
			t.Fatalf("second Close() = %v", err)
		}
	})

	t.Run("API-4 Close of a breaker without a store does nothing", func(t *testing.T) {
		b, _ := newTestBreaker(t)
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("API-4 Close in local mode sends no report", func(t *testing.T) {
		store := liveStore()
		store.down.Store(true)
		b := newLiveBreaker(t, store, WithWindowSize(10*time.Minute), WithSyncInterval(time.Minute))
		before := store.operations.Load()
		if err := b.Close(context.Background()); err != nil || b.Mode() != Local {
			t.Fatalf("Close() = %v in %v mode", err, b.Mode())
		}
		if got := store.operations.Load(); got != before {
			t.Fatalf("store operations went from %d to %d", before, got)
		}
	})

	t.Run("API-3 Close returns when its context ends", func(t *testing.T) {
		store := liveStore()
		b := newLiveBreaker(t, store, WithWindowSize(10*time.Minute), WithSyncInterval(time.Minute), WithStoreTimeout(5*time.Second))
		store.hang.Store(true)
		before := store.operations.Load()
		runTimes(t, b, 1, errDependency)
		eventually(t, "the synchronizer is in a store operation", func() bool { return store.operations.Load() > before })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := b.Close(ctx); err != context.Canceled {
			t.Fatalf("Close() = %v, want context.Canceled", err)
		}
		store.hang.Store(false)
	})
}
