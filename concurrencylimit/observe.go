package concurrencylimit

import (
	"context"
	"log/slog"
)

type EventKind uint8

const LimitChanged EventKind = iota

func (k EventKind) String() string {
	if k == LimitChanged {
		return "limit_changed"
	}
	return "unknown"
}

type Event struct {
	Kind EventKind
	From int
	To   int
}

type Observer interface {
	Observe(Event)
}

func (l *Limiter) emit(event Event) {
	l.log(event)
	if l.cfg.observer == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			l.cfg.logger.Error("concurrency limiter observer panic", "panic", r)
		}
	}()
	l.cfg.observer.Observe(event)
}

func (l *Limiter) log(event Event) {
	recovered := event.To >= l.cfg.initialLimit
	if !recovered && event.To != l.cfg.minLimit || !l.reduced.CompareAndSwap(recovered, !recovered) {
		return
	}
	level, message := slog.LevelWarn, "concurrency limiter limit reached the minimum"
	if recovered {
		level, message = slog.LevelInfo, "concurrency limiter limit returned to the initial limit"
	}
	l.cfg.logger.LogAttrs(context.Background(), level, message, slog.Int("from", event.From), slog.Int("to", event.To))
}
