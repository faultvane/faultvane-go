package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.DiscardHandler)

type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) Observe(event Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, event)
}

func (e *events) take() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	list := e.list
	e.list = nil
	return list
}

func assertEvents(t *testing.T, observer *events, want ...Event) {
	t.Helper()
	if got := observer.take(); !slices.Equal(got, want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
}

type panicking struct{}

func (panicking) Observe(Event) { panic("observer failed") }

type blocking struct {
	entered chan struct{}
	release chan struct{}
}

func (b blocking) Observe(Event) {
	b.entered <- struct{}{}
	<-b.release
}

func synchronizeTimes(limiter *Limiter, times int) {
	for range times {
		synchronize(limiter)
	}
}

func TestEvents(t *testing.T) {
	fault := func(mode Mode) Event {
		return Event{Kind: StoreFault, Limiter: sharedName, Mode: mode, Err: errStoreDown}
	}
	changed := func(mode Mode, cause error) Event {
		return Event{Kind: ModeChanged, Limiter: sharedName, Mode: mode, Err: cause}
	}

	t.Run("RL-43 RL-44 the observer gets each store fault before local mode and each mode change", func(t *testing.T) {
		f, observer := newSharedFixture(t), &events{}
		limiter := f.limiter(WithObserver(observer))
		assertEvents(t, observer)
		f.store.down.Store(true)
		synchronizeTimes(limiter, 2)
		assertEvents(t, observer, fault(Distributed), fault(Distributed))
		synchronize(limiter)
		assertEvents(t, observer, fault(Local), changed(Local, errStoreDown))
		f.store.down.Store(false)
		synchronize(limiter)
		assertEvents(t, observer, changed(Distributed, nil))
		synchronize(limiter)
		assertEvents(t, observer)
	})

	t.Run("RL-44 TK-42 a failed synchronization in local mode gives no event", func(t *testing.T) {
		f, observer := newSharedFixture(t), &events{}
		limiter := f.limiter(WithObserver(observer), WithFaultThreshold(1))
		f.store.down.Store(true)
		synchronize(limiter)
		assertEvents(t, observer, fault(Local), changed(Local, errStoreDown))
		synchronizeTimes(limiter, 1000)
		assertEvents(t, observer)
		assertStats(t, limiter, Stats{Mode: Local, StoreErrors: 1001, Allowance: 1})
	})

	t.Run("RL-43 RL-44 a limiter that starts without the store reports the fault and local mode", func(t *testing.T) {
		f, observer := newSharedFixture(t), &events{}
		f.store.down.Store(true)
		limiter := f.limiter(WithObserver(observer))
		assertEvents(t, observer, fault(Local), changed(Local, errStoreDown))
		synchronizeTimes(limiter, 5)
		assertEvents(t, observer)
		f.store.down.Store(false)
		synchronize(limiter)
		assertEvents(t, observer, changed(Distributed, nil))
	})

	t.Run("RL-43 Close reports local mode without a cause one time", func(t *testing.T) {
		f, observer := newSharedFixture(t), &events{}
		limiter := f.limiter(WithObserver(observer))
		for range 2 {
			if err := limiter.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		assertEvents(t, observer, changed(Local, nil))
	})

	t.Run("RL-43 Close of a limiter in local mode gives no event", func(t *testing.T) {
		f, observer := newSharedFixture(t), &events{}
		f.store.down.Store(true)
		limiter := f.limiter(WithObserver(observer))
		observer.take()
		if err := limiter.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertEvents(t, observer)
	})

	t.Run("RL-43 TK-14 the synchronizer reports the mode changes during calls", func(t *testing.T) {
		var down atomic.Bool
		observer := &events{}
		store := storeFunc(func(context.Context) (Grant, error) {
			if down.Load() {
				return Grant{}, errStoreDown
			}
			return Grant{Tokens: 1}, nil
		})
		limiter, err := New(WithStore(store), WithName(sharedName), WithRate(1000), WithBurst(100), WithLogger(quiet),
			WithObserver(observer), WithSyncInterval(2*time.Millisecond), WithStoreTimeout(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Close(context.Background()) }()
		want := []Event{fault(Distributed), fault(Distributed), fault(Local), changed(Local, errStoreDown), changed(Distributed, nil)}
		var seen []Event
		for _, step := range []struct {
			down   bool
			events int
		}{{true, 4}, {false, 5}} {
			down.Store(step.down)
			eventually(t, "the observer gets the mode change", func() bool {
				limiter.Allow()
				seen = append(seen, observer.take()...)
				return len(seen) >= step.events
			})
		}
		if !slices.Equal(seen, want) {
			t.Fatalf("events = %+v, want %+v", seen, want)
		}
	})

	t.Run("RL-42 the names of the event kinds are fixed", func(t *testing.T) {
		names := []string{ModeChanged.String(), StoreFault.String(), EventKind(99).String()}
		if want := []string{"mode_changed", "store_fault", "unknown"}; !slices.Equal(names, want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
	})
}

func TestLogs(t *testing.T) {
	t.Run("RL-45 TK-42 TK-43 the limiter logs each store fault before local mode and each mode change", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f := newSharedFixture(t)
		limiter := f.limiter(WithLogger(logger))
		call(limiter, 100)
		f.store.down.Store(true)
		synchronizeTimes(limiter, 1000)
		f.store.down.Store(false)
		synchronize(limiter)
		if err := limiter.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, raw := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			record := map[string]any{}
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				t.Fatal(err)
			}
			delete(record, "time")
			line, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(line))
		}
		failed := `{"cause":"store down","level":"DEBUG","limiter":"orders","mode":"distributed","msg":"rate limiter synchronization failed"}`
		want := []string{
			failed,
			failed,
			`{"cause":"store down","level":"DEBUG","limiter":"orders","mode":"local","msg":"rate limiter synchronization failed"}`,
			`{"cause":"store down","level":"WARN","limiter":"orders","mode":"local","msg":"rate limiter mode changed"}`,
			`{"level":"INFO","limiter":"orders","mode":"distributed","msg":"rate limiter mode changed"}`,
			`{"level":"INFO","limiter":"orders","mode":"local","msg":"rate limiter mode changed"}`,
		}
		if !slices.Equal(lines, want) {
			t.Fatalf("log lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
	})

	t.Run("RL-46 a panic of the observer is logged and does not stop the limiter", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
		f := newSharedFixture(t)
		f.store.down.Store(true)
		limiter := f.limiter(WithLogger(logger), WithObserver(panicking{}))
		want := `"level":"ERROR","msg":"rate limiter observer panic","limiter":"orders","panic":"observer failed"}` + "\n"
		if got := output.String(); strings.Count(got, want) != 2 || strings.Count(got, "\n") != 2 {
			t.Fatalf("log = %q, want two times %q", got, want)
		}
		f.store.down.Store(false)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Distributed, StoreErrors: 1, Allowance: 1})
	})
}

func TestObserverIsolation(t *testing.T) {
	t.Run("RL-46 TK-33 an observer that does not return blocks no call", func(t *testing.T) {
		f := newSharedFixture(t)
		observer := blocking{entered: make(chan struct{}), release: make(chan struct{})}
		limiter := f.limiter(WithObserver(observer), WithFaultThreshold(1))
		f.store.down.Store(true)
		synchronized := make(chan struct{})
		go func() {
			defer close(synchronized)
			synchronize(limiter)
		}()
		<-observer.entered
		decided := make(chan Stats, 1)
		go func() {
			limiter.Allow()
			decided <- limiter.Stats()
		}()
		select {
		case stats := <-decided:
			if want := (Stats{Mode: Local, Permitted: 1, StoreErrors: 1}); stats != want {
				t.Errorf("stats = %+v, want %+v", stats, want)
			}
		case <-time.After(15 * time.Second):
			t.Error("the call waits for the observer")
		}
		close(observer.release)
		<-observer.entered
		<-synchronized
	})

	t.Run("RL-47 RL-41 TK-42 TK-50 a call gives no event and no log and makes no allocation", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f, observer := newSharedFixture(t), &events{}
		limiter := f.limiter(WithRate(1e6), WithBurst(2000), WithObserver(observer), WithLogger(logger))
		ctx := context.Background()
		run := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Do(ctx, run) }); allocations != 0 {
			t.Errorf("Do: %v allocations for each call, want 0", allocations)
		}
		assertPermitted(t, limiter, int(limiter.Stats().Allowance))
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Allow() }); allocations != 0 {
			t.Errorf("rejected Allow: %v allocations for each call, want 0", allocations)
		}
		if stats := limiter.Stats(); stats.Permitted != 2000 || stats.Rejected < 1000 {
			t.Fatalf("stats = %+v, want 2000 permitted calls and 1000 or more rejected calls", stats)
		}
		if got := observer.take(); len(got) != 0 || output.Len() != 0 {
			t.Fatalf("events = %+v, log = %q, want none", got, output.String())
		}
	})
}
