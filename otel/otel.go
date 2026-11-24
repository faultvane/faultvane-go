package faultvaneotel

import (
	"context"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const prefix = "faultvane.circuit_breaker."

var (
	nameKey      = attribute.Key("faultvane.breaker.name")
	stateKey     = attribute.Key("faultvane.breaker.state")
	modeKey      = attribute.Key("faultvane.breaker.mode")
	decisionKey  = attribute.Key("faultvane.breaker.decision")
	reasonKey    = attribute.Key("faultvane.breaker.rejection_reason")
	resultKey    = attribute.Key("result")
	toKey        = attribute.Key("to")
	operationKey = attribute.Key("operation")
	errorKey     = attribute.Key("error")
	states       = []circuitbreaker.State{circuitbreaker.Closed, circuitbreaker.Open, circuitbreaker.HalfOpen}
	modes        = []circuitbreaker.Mode{circuitbreaker.Distributed, circuitbreaker.Local}
)

type instruments struct {
	state           metric.Int64ObservableGauge
	mode            metric.Int64ObservableGauge
	expired         metric.Int64ObservableGauge
	calls           metric.Int64ObservableCounter
	slowCalls       metric.Int64ObservableCounter
	rejected        metric.Int64ObservableCounter
	probes          metric.Int64ObservableCounter
	transitions     metric.Int64ObservableCounter
	storeErrors     metric.Int64ObservableCounter
	localMode       metric.Int64ObservableCounter
	reconciliations metric.Int64ObservableCounter
}

func flag(on bool) int64 {
	if on {
		return 1
	}
	return 0
}

func newInstruments(meter metric.Meter) (instruments, []metric.Observable, error) {
	var i instruments
	var all []metric.Observable
	var failure error
	gauge := func(name, description string) metric.Int64ObservableGauge {
		g, err := meter.Int64ObservableGauge(prefix+name, metric.WithDescription(description))
		if err != nil {
			failure = err
		}
		all = append(all, g)
		return g
	}
	counter := func(name, description string) metric.Int64ObservableCounter {
		c, err := meter.Int64ObservableCounter(prefix+name, metric.WithDescription(description))
		if err != nil {
			failure = err
		}
		all = append(all, c)
		return c
	}
	i.state = gauge("state", "1 for the current state of the circuit breaker, 0 for the other states")
	i.mode = gauge("mode", "1 for the current mode of the circuit breaker, 0 for the other mode")
	i.expired = gauge("local_mode_expired", "1 when local mode is longer than the maximum time")
	i.calls = counter("calls", "Calls that the circuit breaker permitted, by result")
	i.slowCalls = counter("slow_calls", "Calls that were slow")
	i.rejected = counter("rejected_calls", "Calls that the circuit breaker rejected, by reason")
	i.probes = counter("probes", "Results of probes, by result")
	i.transitions = counter("transitions", "State transitions that this instance observed, by new state")
	i.storeErrors = counter("store_errors", "Store operations that failed")
	i.localMode = counter("local_mode", "Changes to local mode")
	i.reconciliations = counter("reconciliations", "Reconciliations with the store")
	return i, all, failure
}

func (i instruments) observe(observer metric.Observer, breaker *circuitbreaker.Breaker) {
	stats := breaker.Stats()
	name := nameKey.String(breaker.Name())
	with := func(extra ...attribute.KeyValue) metric.ObserveOption {
		return metric.WithAttributes(append([]attribute.KeyValue{name}, extra...)...)
	}
	for _, state := range states {
		observer.ObserveInt64(i.state, flag(stats.State == state), with(stateKey.String(state.String())))
	}
	for _, mode := range modes {
		observer.ObserveInt64(i.mode, flag(stats.Mode == mode), with(modeKey.String(mode.String())))
	}
	observer.ObserveInt64(i.expired, flag(stats.LocalModeExpired), with())
	observer.ObserveInt64(i.calls, stats.Successes, with(resultKey.String("success")))
	observer.ObserveInt64(i.calls, stats.Failures, with(resultKey.String("failure")))
	observer.ObserveInt64(i.calls, stats.Ignored, with(resultKey.String("ignored")))
	observer.ObserveInt64(i.slowCalls, stats.SlowCalls, with())
	observer.ObserveInt64(i.rejected, stats.RejectedOpen, with(reasonKey.String("open")))
	observer.ObserveInt64(i.rejected, stats.RejectedProbe, with(reasonKey.String("probe_not_permitted")))
	observer.ObserveInt64(i.probes, stats.ProbeSuccesses, with(resultKey.String("success")))
	observer.ObserveInt64(i.probes, stats.ProbeFailures, with(resultKey.String("failure")))
	observer.ObserveInt64(i.transitions, stats.Opened, with(toKey.String("open")))
	observer.ObserveInt64(i.transitions, stats.HalfOpened, with(toKey.String("half_open")))
	observer.ObserveInt64(i.transitions, stats.Closed, with(toKey.String("closed")))
	observer.ObserveInt64(i.storeErrors, stats.StoreErrors, with())
	observer.ObserveInt64(i.localMode, stats.LocalModeEntries, with())
	observer.ObserveInt64(i.reconciliations, stats.Reconciliations, with())
}

func RegisterMetrics(meter metric.Meter, breakers ...*circuitbreaker.Breaker) (metric.Registration, error) {
	i, all, err := newInstruments(meter)
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		for _, breaker := range breakers {
			i.observe(observer, breaker)
		}
		return nil
	}, all...)
}

type traced struct {
	breaker *circuitbreaker.Breaker
}

func Traced(breaker *circuitbreaker.Breaker) faultvane.Policy {
	return traced{breaker: breaker}
}

func (t traced) Do(ctx context.Context, call func(context.Context) error) error {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return t.breaker.Do(ctx, call)
	}
	state := t.breaker.State()
	ran := false
	err := t.breaker.Do(ctx, func(ctx context.Context) error {
		ran = true
		return call(ctx)
	})
	attributes := []attribute.KeyValue{
		nameKey.String(t.breaker.Name()),
		stateKey.String(state.String()),
		modeKey.String(t.breaker.Mode().String()),
	}
	switch {
	case ran && state == circuitbreaker.Closed:
		attributes = append(attributes, decisionKey.String("permitted"))
	case ran:
		attributes = append(attributes, decisionKey.String("probe"))
	case err == circuitbreaker.ErrCircuitOpen:
		attributes = append(attributes, decisionKey.String("rejected"), reasonKey.String("open"))
	case err == circuitbreaker.ErrProbeNotPermitted:
		attributes = append(attributes, decisionKey.String("rejected"), reasonKey.String("probe_not_permitted"))
	}
	span.SetAttributes(attributes...)
	return err
}

type instrumentedStore struct {
	store    circuitbreaker.Store
	duration metric.Float64Histogram
}

func InstrumentStore(store circuitbreaker.Store, meter metric.Meter) (circuitbreaker.Store, error) {
	duration, err := meter.Float64Histogram(prefix+"store.duration", metric.WithUnit("s"), metric.WithDescription("Duration of store operations"))
	if err != nil {
		return nil, err
	}
	return instrumentedStore{store: store, duration: duration}, nil
}

func (s instrumentedStore) record(ctx context.Context, operation string, started time.Time, err error) {
	s.duration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(operationKey.String(operation), errorKey.Bool(err != nil)))
}

func (s instrumentedStore) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	started := time.Now()
	snapshot, err := s.store.Load(ctx, name)
	s.record(ctx, "load", started, err)
	return snapshot, err
}

func (s instrumentedStore) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	started := time.Now()
	snapshot, err := s.store.Report(ctx, name, report)
	s.record(ctx, "report", started, err)
	return snapshot, err
}

func (s instrumentedStore) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	started := time.Now()
	grant, err := s.store.AcquireProbe(ctx, name, request)
	s.record(ctx, "acquire_probe", started, err)
	return grant, err
}

func (s instrumentedStore) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	started := time.Now()
	snapshot, err := s.store.CompleteProbe(ctx, name, result)
	s.record(ctx, "complete_probe", started, err)
	return snapshot, err
}

func (s instrumentedStore) Restore(ctx context.Context, name string, snapshot circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	started := time.Now()
	restored, err := s.store.Restore(ctx, name, snapshot)
	s.record(ctx, "restore", started, err)
	return restored, err
}
