package circuitbreaker

import (
	"context"
	"log/slog"
	"sync/atomic"
)

type Mode uint8

const (
	Distributed Mode = iota
	Local
)

func (m Mode) String() string {
	if m == Distributed {
		return "distributed"
	}
	return "local"
}

type EventKind uint8

const (
	StateChanged EventKind = iota
	ModeChanged
	Reconciled
	StoreFault
	LocalModeExpired
	ConfigMismatchChanged
)

func (k EventKind) String() string {
	switch k {
	case StateChanged:
		return "state_changed"
	case ModeChanged:
		return "mode_changed"
	case Reconciled:
		return "reconciled"
	case StoreFault:
		return "store_fault"
	case LocalModeExpired:
		return "local_mode_expired"
	case ConfigMismatchChanged:
		return "config_mismatch_changed"
	}
	return "unknown"
}

type Event struct {
	Kind           EventKind
	Breaker        string
	From           State
	To             State
	Generation     uint64
	Mode           Mode
	Err            error
	ConfigMismatch bool
}

type Observer interface {
	Observe(Event)
}

type Stats struct {
	State            State
	Mode             Mode
	Successes        int64
	Failures         int64
	Ignored          int64
	SlowCalls        int64
	RejectedOpen     int64
	RejectedProbe    int64
	ProbeSuccesses   int64
	ProbeFailures    int64
	Opened           int64
	HalfOpened       int64
	Closed           int64
	StoreErrors      int64
	LocalModeEntries int64
	Reconciliations  int64
	LocalModeExpired bool
	ConfigMismatch   bool
}

type counters struct {
	successes        atomic.Int64
	failures         atomic.Int64
	ignored          atomic.Int64
	slowCalls        atomic.Int64
	rejectedOpen     atomic.Int64
	rejectedProbe    atomic.Int64
	probeSuccesses   atomic.Int64
	probeFailures    atomic.Int64
	opened           atomic.Int64
	halfOpened       atomic.Int64
	closed           atomic.Int64
	storeErrors      atomic.Int64
	localModeEntries atomic.Int64
	reconciliations  atomic.Int64
	localModeExpired atomic.Bool
	configMismatch   atomic.Bool
}

func (b *Breaker) Stats() Stats {
	c := &b.counters
	return Stats{
		State:            b.State(),
		Mode:             b.Mode(),
		Successes:        c.successes.Load(),
		Failures:         c.failures.Load(),
		Ignored:          c.ignored.Load(),
		SlowCalls:        c.slowCalls.Load(),
		RejectedOpen:     c.rejectedOpen.Load(),
		RejectedProbe:    c.rejectedProbe.Load(),
		ProbeSuccesses:   c.probeSuccesses.Load(),
		ProbeFailures:    c.probeFailures.Load(),
		Opened:           c.opened.Load(),
		HalfOpened:       c.halfOpened.Load(),
		Closed:           c.closed.Load(),
		StoreErrors:      c.storeErrors.Load(),
		LocalModeEntries: c.localModeEntries.Load(),
		Reconciliations:  c.reconciliations.Load(),
		LocalModeExpired: c.localModeExpired.Load(),
		ConfigMismatch:   c.configMismatch.Load(),
	}
}

func (b *Breaker) emit(event Event) {
	event.Breaker = b.name
	b.log(event)
	if b.cfg.observer == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			b.cfg.logger.Error("circuit breaker observer panic", "breaker", b.name, "panic", r)
		}
	}()
	b.cfg.observer.Observe(event)
}

func (b *Breaker) log(event Event) {
	level, message := slog.LevelInfo, ""
	switch event.Kind {
	case StateChanged:
		message = "circuit breaker state changed"
		if event.To == Open {
			level = slog.LevelWarn
		}
	case ModeChanged:
		message = "circuit breaker mode changed"
		if event.Mode == Local {
			level = slog.LevelWarn
		}
	case Reconciled:
		message = "circuit breaker reconciled with the store"
	case StoreFault:
		level, message = slog.LevelDebug, "circuit breaker store operation failed"
	case LocalModeExpired:
		level, message = slog.LevelError, "circuit breaker local mode is longer than the maximum time"
	case ConfigMismatchChanged:
		message = "circuit breaker configuration is equal to the configuration of the last writer again"
		if event.ConfigMismatch {
			level, message = slog.LevelWarn, "circuit breaker configuration is different from the configuration of the last writer"
		}
	}
	attrs := []slog.Attr{
		slog.String("breaker", event.Breaker),
		slog.String("from", event.From.String()),
		slog.String("to", event.To.String()),
		slog.Uint64("generation", event.Generation),
		slog.String("mode", event.Mode.String()),
	}
	if event.Kind == ConfigMismatchChanged {
		attrs = append(attrs, slog.String("fingerprint", b.fingerprint), slog.String("stored_fingerprint", b.auth.Fingerprint))
	}
	if event.Err != nil {
		attrs = append(attrs, slog.String("cause", event.Err.Error()))
	}
	b.cfg.logger.LogAttrs(context.Background(), level, message, attrs...)
}
