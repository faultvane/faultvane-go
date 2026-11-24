package ratelimit

import (
	"context"
	"log/slog"
)

type EventKind uint8

const (
	ModeChanged EventKind = iota
	StoreFault
)

func (k EventKind) String() string {
	switch k {
	case ModeChanged:
		return "mode_changed"
	case StoreFault:
		return "store_fault"
	}
	return "unknown"
}

type Event struct {
	Kind    EventKind
	Limiter string
	Mode    Mode
	Err     error
}

type Observer interface {
	Observe(Event)
}

func (l *Limiter) report(cause error) {
	s := l.shared
	l.mu.Lock()
	mode, reported := s.mode, s.reported
	s.reported = mode
	l.mu.Unlock()
	if cause != nil && reported == Distributed {
		s.emit(Event{Kind: StoreFault, Mode: mode, Err: cause})
	}
	if mode != reported {
		s.emit(Event{Kind: ModeChanged, Mode: mode, Err: cause})
	}
}

func (s *shared) emit(event Event) {
	event.Limiter = s.cfg.name
	s.log(event)
	if s.cfg.observer == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.cfg.logger.Error("rate limiter observer panic", "limiter", s.cfg.name, "panic", r)
		}
	}()
	s.cfg.observer.Observe(event)
}

func (s *shared) log(event Event) {
	level, message := slog.LevelInfo, "rate limiter mode changed"
	switch {
	case event.Kind == StoreFault:
		level, message = slog.LevelDebug, "rate limiter synchronization failed"
	case event.Err != nil:
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("limiter", event.Limiter),
		slog.String("mode", event.Mode.String()),
	}
	if event.Err != nil {
		attrs = append(attrs, slog.String("cause", event.Err.Error()))
	}
	s.cfg.logger.LogAttrs(context.Background(), level, message, attrs...)
}
