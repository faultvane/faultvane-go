package concurrencylimit

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

func changed(from, to int) Event {
	return Event{Kind: LimitChanged, From: from, To: to}
}

type counting struct{ events atomic.Int64 }

func (c *counting) Observe(Event) { c.events.Add(1) }

type panicking struct{}

func (panicking) Observe(Event) { panic("observer failed") }

type logRecord struct {
	Level, Msg string
	From, To   int
}

func recordingLogger() (*slog.Logger, func(*testing.T) []logRecord) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, func(t *testing.T) []logRecord {
		t.Helper()
		var records []logRecord
		for _, raw := range strings.Split(output.String(), "\n") {
			if raw == "" {
				continue
			}
			fields := map[string]any{}
			if err := json.Unmarshal([]byte(raw), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 5 {
				t.Fatalf("log record %s has fields other than time, level, msg, from, and to", raw)
			}
			var record logRecord
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		output.Reset()
		return records
	}
}

func assertRecords(t *testing.T, got []logRecord, want ...logRecord) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("log records = %+v, want %+v", got, want)
	}
}

func reached(from, to int) logRecord {
	return logRecord{"WARN", "concurrency limiter limit reached the minimum", from, to}
}

func returned(from, to int) logRecord {
	return logRecord{"INFO", "concurrency limiter limit returned to the initial limit", from, to}
}

func TestEvents(t *testing.T) {
	t.Run("CL-27 the observer gets each change of the effective limit", func(t *testing.T) {
		observer := &events{}
		l, _ := newLimiter(t, WithInitialLimit(2), WithMaxLimit(3), WithObserver(observer))
		runTimes(t, l, 2, nil)
		assertEvents(t, observer)
		runTimes(t, l, 1, nil)
		assertEvents(t, observer, changed(2, 3))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertEvents(t, observer, changed(3, 2))
		runTimes(t, l, 2, context.DeadlineExceeded)
		assertEvents(t, observer)
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertEvents(t, observer, changed(2, 1))
	})

	t.Run("CL-27 a call that does not change the effective limit gives no event", func(t *testing.T) {
		observer := &events{}
		l, _ := newLimiter(t, append(fixedLimit(1), WithObserver(observer))...)
		runTimes(t, l, 3, nil)
		runTimes(t, l, 3, context.DeadlineExceeded)
		holding(l, 1, func() { assertRejected(t, l) })
		assertEvents(t, observer)
	})

	t.Run("CL-27 CL-17 concurrent changes give one event for each change", func(t *testing.T) {
		observer := &events{}
		const signals = 10
		l, _ := newLimiter(t, WithInitialLimit(1<<20), WithMaxLimit(1<<20), WithBackoffRatio(0.5), WithObserver(observer))
		var group sync.WaitGroup
		for range signals {
			group.Add(1)
			go func() {
				defer group.Done()
				_ = run(l, context.DeadlineExceeded)
			}()
		}
		group.Wait()
		got := observer.take()
		slices.SortFunc(got, func(a, b Event) int { return b.From - a.From })
		var want []Event
		for step := range signals {
			want = append(want, changed(1<<20>>step, 1<<20>>(step+1)))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	})

	t.Run("CL-27 the name of the event kind is fixed", func(t *testing.T) {
		if got := []string{LimitChanged.String(), EventKind(99).String()}; !slices.Equal(got, []string{"limit_changed", "unknown"}) {
			t.Fatalf("names = %v", got)
		}
	})

	t.Run("CL-29 a panic of the observer is logged and does not reach the caller", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
		l, _ := newLimiter(t, WithBackoffRatio(0.5), WithLogger(logger), WithObserver(panicking{}))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertLimit(t, l, 10)
		if l.InFlight() != 0 {
			t.Fatalf("InFlight() = %d, want 0", l.InFlight())
		}
		want := `"level":"ERROR","msg":"concurrency limiter observer panic","panic":"observer failed"}` + "\n"
		if got := output.String(); !strings.HasSuffix(got, want) || strings.Count(got, "\n") != 1 {
			t.Fatalf("log = %q, want one record with %q", got, want)
		}
	})
}

func TestLogs(t *testing.T) {
	t.Run("CL-28 TK-42 TK-43 the limiter logs when the limit reaches the minimum and when it returns to the initial limit", func(t *testing.T) {
		logger, records := recordingLogger()
		l, _ := newLimiter(t, WithInitialLimit(3), WithMaxLimit(4), WithLogger(logger))
		runTimes(t, l, 3, context.DeadlineExceeded)
		assertRecords(t, records(t))
		runTimes(t, l, 1, context.DeadlineExceeded)
		assertRecords(t, records(t), reached(2, 1))
		runTimes(t, l, 20, context.DeadlineExceeded)
		runTimes(t, l, 3, nil)
		assertRecords(t, records(t))
		runTimes(t, l, 1, nil)
		assertRecords(t, records(t), returned(2, 3))
		holding(l, 1, func() { runTimes(t, l, 4, nil) })
		assertLimit(t, l, 4)
		assertRecords(t, records(t))
		runTimes(t, l, 20, context.DeadlineExceeded)
		assertRecords(t, records(t), reached(2, 1))
	})

	t.Run("CL-28 TK-42 a limit that goes to the minimum and leaves it many times gives one log record", func(t *testing.T) {
		logger, records := recordingLogger()
		observer := &events{}
		l, _ := newLimiter(t, WithLogger(logger), WithObserver(observer))
		runTimes(t, l, 40, context.DeadlineExceeded)
		observer.take()
		for range 1000 {
			runTimes(t, l, 1, nil)
			runTimes(t, l, 3, context.DeadlineExceeded)
		}
		list := observer.take()
		atMinimum := 0
		for _, event := range list {
			if event.To == 1 {
				atMinimum++
			}
		}
		if len(list) != 2000 || atMinimum != 1000 {
			t.Fatalf("events = %d, events that reach the minimum = %d, want 2000 and 1000", len(list), atMinimum)
		}
		assertRecords(t, records(t), reached(2, 1))
	})

	t.Run("CL-28 a decrease from above the initial limit to the minimum and the return give two log records", func(t *testing.T) {
		logger, records := recordingLogger()
		observer := &counting{}
		l, _ := newLimiter(t, WithInitialLimit(200), WithLogger(logger), WithObserver(observer))
		runTimes(t, l, 60, context.DeadlineExceeded)
		assertLimit(t, l, 1)
		for l.Limit() < 200 {
			holding(l, l.Limit()/2, func() { runTimes(t, l, 1, nil) })
		}
		if got := observer.events.Load(); got != 236 {
			t.Fatalf("events = %d, want 236", got)
		}
		assertRecords(t, records(t), reached(2, 1), returned(199, 200))
	})

	t.Run("CL-28 a limiter with the initial limit at the minimum writes no log", func(t *testing.T) {
		logger, records := recordingLogger()
		observer := &events{}
		l, _ := newLimiter(t, WithInitialLimit(1), WithMaxLimit(2), WithLogger(logger), WithObserver(observer))
		runTimes(t, l, 1, nil)
		runTimes(t, l, 10, context.DeadlineExceeded)
		runTimes(t, l, 1, nil)
		assertEvents(t, observer, changed(1, 2), changed(2, 1), changed(1, 2))
		assertRecords(t, records(t))
	})
}

func TestObservedResources(t *testing.T) {
	t.Run("CL-30 CL-23 TK-42 TK-50 a call that does not change the effective limit gives no event and no log and makes no allocation", func(t *testing.T) {
		ctx := context.Background()
		overloaded := func(context.Context) error { return context.DeadlineExceeded }
		for name, c := range map[string]struct {
			opts []Option
			call func(context.Context) error
		}{
			"call that is not busy":    {nil, succeed},
			"busy call at the maximum": {fixedLimit(1), succeed},
			"overload at the minimum":  {fixedLimit(1), overloaded},
			"increase of the fraction": {[]Option{WithInitialLimit(2000), WithMaxLimit(4000), WithMinLimit(1999)}, succeed},
			"rejection":                {fixedLimit(1), nil},
		} {
			logger, records := recordingLogger()
			observer := &counting{}
			l, _ := newLimiter(t, append(c.opts, WithLogger(logger), WithObserver(observer))...)
			measured := func() { _ = l.Do(ctx, c.call) }
			if c.call == nil {
				measured = func() { holding(l, 1, func() { _ = l.Do(ctx, succeed) }) }
			}
			if allocations := testing.AllocsPerRun(1000, measured); allocations != 0 {
				t.Errorf("%s: %v allocations for each call, want 0", name, allocations)
			}
			if got := records(t); observer.events.Load() != 0 || len(got) != 0 {
				t.Errorf("%s: events = %d, log records = %+v, want none", name, observer.events.Load(), got)
			}
		}
	})

	t.Run("CL-30 TK-50 a change of the limit that gives no log makes no allocation", func(t *testing.T) {
		logger, records := recordingLogger()
		observer := &counting{}
		l, _ := newLimiter(t, WithInitialLimit(1), WithMaxLimit(1<<20), WithLogger(logger), WithObserver(observer))
		ctx := context.Background()
		if allocations := testing.AllocsPerRun(1000, func() { _ = l.Do(ctx, succeed) }); allocations != 0 {
			t.Errorf("%v allocations for each call, want 0", allocations)
		}
		if got := records(t); observer.events.Load() == 0 || len(got) != 0 {
			t.Errorf("events = %d, log records = %+v, want events and no log record", observer.events.Load(), got)
		}
	})
}
