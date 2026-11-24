package circuitbreaker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errSubscribe = errors.New("subscription refused")

type notifyingStore struct {
	*testStore
	fails         error
	hangs         bool
	mu            sync.Mutex
	changed       func()
	subscribed    string
	subscriptions atomic.Int64
	stops         atomic.Int64
}

func newNotifyingStore() *notifyingStore {
	return &notifyingStore{testStore: liveStore()}
}

func (s *notifyingStore) Subscribe(ctx context.Context, name string, changed func()) (func(), error) {
	defer s.subscriptions.Add(1)
	if s.hangs {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.fails != nil {
		return nil, s.fails
	}
	s.mu.Lock()
	s.changed, s.subscribed = changed, name
	s.mu.Unlock()
	return func() { s.stops.Add(1) }, nil
}

func (s *notifyingStore) notify() {
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	changed()
}

func newNotifiedBreaker(t *testing.T, store *notifyingStore, opts ...Option) *Breaker {
	t.Helper()
	base := []Option{WithWindowSize(10 * time.Minute), WithSyncInterval(time.Minute)}
	b := newLiveBreaker(t, store, append(base, opts...)...)
	eventually(t, "the breaker asked for a subscription", func() bool { return store.subscriptions.Load() == 1 })
	return b
}

func TestNotifications(t *testing.T) {
	t.Run("DS-10 a notification brings a transition to an instance before the sync interval ends", func(t *testing.T) {
		writerStore, readerStore := newNotifyingStore(), newNotifyingStore()
		readerStore.Store = writerStore.Store
		writer, reader := newNotifiedBreaker(t, writerStore), newNotifiedBreaker(t, readerStore)
		runTimes(t, writer, 5, errDependency)
		eventually(t, "the writer is open", func() bool { return writer.State() == Open })
		if reader.State() != Closed {
			t.Fatalf("the reader is %v inside the sync interval without a notification, want closed", reader.State())
		}
		readerStore.notify()
		eventually(t, "the reader is open", func() bool { return reader.State() == Open })
		if readerStore.subscribed != sharedName {
			t.Fatalf("the breaker subscribed to %q, want %q", readerStore.subscribed, sharedName)
		}
		if s := reader.Stats(); s.Notifications != 1 || s.SubscribeFailed || reader.current.Load().generation != 1 {
			t.Fatalf("stats = %+v, want 1 notification, no failed subscription, and generation 1", s)
		}
	})

	t.Run("DS-11 a false and a repeated notification change no state", func(t *testing.T) {
		store := newNotifyingStore()
		b := newNotifiedBreaker(t, store)
		before := store.operations.Load()
		for range 3 {
			store.notify()
		}
		eventually(t, "the breaker read the store", func() bool { return store.operations.Load() > before })
		if v := b.current.Load(); v.state != Closed || v.generation != 0 || v.local {
			t.Fatalf("the breaker is %v generation %d in %v mode, want closed generation 0 in distributed mode", v.state, v.generation, b.Mode())
		}
		if err := run(b, nil); err != nil {
			t.Fatalf("Do() = %v after false notifications", err)
		}
	})

	t.Run("DS-11 an instance without a notification gets the transition at the sync interval", func(t *testing.T) {
		writerStore, readerStore := newNotifyingStore(), newNotifyingStore()
		readerStore.Store = writerStore.Store
		short := WithSyncInterval(20 * time.Millisecond)
		writer, reader := newNotifiedBreaker(t, writerStore, short), newNotifiedBreaker(t, readerStore, short)
		runTimes(t, writer, 5, errDependency)
		eventually(t, "the reader is open", func() bool { return reader.State() == Open })
		if got := reader.Stats().Notifications; got != 0 {
			t.Fatalf("the reader got %d notifications, want 0", got)
		}
	})

	t.Run("PF-7 many notifications inside the early report interval give no store operation", func(t *testing.T) {
		store := newNotifyingStore()
		b := newNotifiedBreaker(t, store, WithEarlyReportInterval(time.Minute))
		before := store.operations.Load()
		store.notify()
		eventually(t, "the synchronizer took the signal", func() bool { return len(b.wake) == 0 })
		for range 1000 {
			store.notify()
		}
		if got := store.operations.Load(); got != before || len(b.wake) != 1 {
			t.Fatalf("store operations went from %d to %d with %d signals, want no operation and 1 signal", before, got, len(b.wake))
		}
		if got := b.Stats().Notifications; got != 1001 {
			t.Fatalf("Stats().Notifications = %d, want 1001", got)
		}
	})

	t.Run("PF-7 a stream of notifications gives at most one store operation in each early report interval", func(t *testing.T) {
		const interval = 15 * time.Millisecond
		store := newNotifyingStore()
		newNotifiedBreaker(t, store, WithEarlyReportInterval(interval))
		before, started := store.operations.Load(), time.Now()
		eventually(t, "5 synchronizations", func() bool {
			store.notify()
			return store.operations.Load()-before >= 5
		})
		operations := store.operations.Load() - before
		if limit := int64(time.Since(started)/interval) + 1; operations > limit {
			t.Fatalf("%d store operations, want at most %d", operations, limit)
		}
	})

	t.Run("DG-8 an instance in local mode ignores a notification", func(t *testing.T) {
		f := newFleet(t, 1)
		b := f.breakers[0]
		b.notified()
		if len(b.wake) != 1 {
			t.Fatal("a notification in distributed mode does not wake the synchronizer")
		}
		<-b.wake
		f.disconnect()
		b.notified()
		if b.Mode() != Local || len(b.wake) != 0 {
			t.Fatalf("a notification in %v mode gave %d signals, want local mode and 0", b.Mode(), len(b.wake))
		}
		if got := b.Stats().Notifications; got != 2 {
			t.Fatalf("Stats().Notifications = %d, want 2", got)
		}
	})

	t.Run("RF-6 ERR-3 a failed subscription fails no call and is visible", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
		store := newNotifyingStore()
		store.fails = errSubscribe
		b := newNotifiedBreaker(t, store, WithLogger(logger), WithSyncInterval(20*time.Millisecond))
		eventually(t, "the failed subscription is in the statistics", func() bool { return b.Stats().SubscribeFailed })
		runTimes(t, b, 5, errDependency)
		eventually(t, "the breaker is open through the sync interval", func() bool { return b.State() == Open && b.Mode() == Distributed })
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		logs := output.String()
		for _, want := range []string{"level=WARN", "subscription to transition notifications failed", "breaker=" + sharedName, errSubscribe.Error()} {
			if !strings.Contains(logs, want) {
				t.Fatalf("logs = %q, want %q", logs, want)
			}
		}
	})

	t.Run("RF-6 RF-2 a subscription that does not answer ends at the store timeout", func(t *testing.T) {
		store := newNotifyingStore()
		store.hangs = true
		b := newNotifiedBreaker(t, store)
		eventually(t, "the failed subscription is in the statistics", func() bool { return b.Stats().SubscribeFailed })
		if err := run(b, nil); err != nil {
			t.Fatalf("Do() = %v, want nil", err)
		}
	})

	t.Run("API-4 Close stops the subscription", func(t *testing.T) {
		store := newNotifyingStore()
		b := newNotifiedBreaker(t, store)
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if subscriptions, stops := store.subscriptions.Load(), store.stops.Load(); subscriptions != 1 || stops != 1 {
			t.Fatalf("%d subscriptions and %d stops after Close, want 1 and 1", subscriptions, stops)
		}
		store.notify()
		if b.State() != Closed {
			t.Fatalf("a notification after Close gave %v, want closed", b.State())
		}
	})

	t.Run("DS-10 a store without notifications gets no subscription", func(t *testing.T) {
		b := newLiveBreaker(t, liveStore())
		if err := b.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s := b.Stats(); s.Notifications != 0 || s.SubscribeFailed {
			t.Fatalf("stats = %+v, want no notification and no failed subscription", s)
		}
	})
}
