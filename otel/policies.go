package faultvaneotel

import (
	"context"

	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/concurrencylimit"
	"github.com/faultvane/faultvane-go/loadshed"
	"github.com/faultvane/faultvane-go/retry"
	"github.com/faultvane/faultvane-go/timeout"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	policyNameKey  = attribute.Key("faultvane.policy.name")
	priorityKey    = attribute.Key("priority")
	priorityLabels = [...]string{loadshed.Critical: "critical", loadshed.High: "high", loadshed.Normal: "normal", loadshed.Low: "low"}
)

type recordFunc func(instrument metric.Int64Observable, value int64, attributes ...attribute.KeyValue)

type policyMetrics struct {
	meter       metric.Meter
	instruments []metric.Observable
	failure     error
}

func (m *policyMetrics) counter(name, description string) metric.Int64ObservableCounter {
	c, err := m.meter.Int64ObservableCounter("faultvane."+name, metric.WithDescription(description))
	if err != nil {
		m.failure = err
	}
	m.instruments = append(m.instruments, c)
	return c
}

func (m *policyMetrics) gauge(name, description string) metric.Int64ObservableGauge {
	g, err := m.meter.Int64ObservableGauge("faultvane."+name, metric.WithDescription(description))
	if err != nil {
		m.failure = err
	}
	m.instruments = append(m.instruments, g)
	return g
}

func (m *policyMetrics) register(name string, observe func(recordFunc)) (metric.Registration, error) {
	if m.failure != nil {
		return nil, m.failure
	}
	policy := policyNameKey.String(name)
	return m.meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		observe(func(instrument metric.Int64Observable, value int64, attributes ...attribute.KeyValue) {
			observer.ObserveInt64(instrument, value, metric.WithAttributes(append(attributes, policy)...))
		})
		return nil
	}, m.instruments...)
}

func RegisterTimeoutMetrics(meter metric.Meter, name string, t *timeout.Timeout) (metric.Registration, error) {
	m := policyMetrics{meter: meter}
	calls := m.counter("timeout.calls", "Calls that the timeout policy ran")
	timeouts := m.counter("timeout.timeouts", "Calls that the time limit ended")
	return m.register(name, func(record recordFunc) {
		stats := t.Stats()
		record(calls, stats.Calls)
		record(timeouts, stats.Timeouts)
	})
}

func RegisterRetryMetrics(meter metric.Meter, name string, r *retry.Retry) (metric.Registration, error) {
	m := policyMetrics{meter: meter}
	calls := m.counter("retry.calls", "Calls that the retry policy ran, counted by the first attempt")
	retries := m.counter("retry.retries", "Attempts after the first attempt")
	failed := m.counter("retry.failed_calls", "Calls that returned an error after their last attempt")
	gateStops := m.counter("retry.gate_stops", "Retries that the gate stopped")
	budgetStops := m.counter("retry.budget_stops", "Retries that an empty budget stopped")
	return m.register(name, func(record recordFunc) {
		stats := r.Stats()
		record(calls, stats.Calls)
		record(retries, stats.Retries)
		record(failed, stats.Failed)
		record(gateStops, stats.GateStops)
		record(budgetStops, stats.BudgetStops)
	})
}

func RegisterBulkheadMetrics(meter metric.Meter, name string, b *bulkhead.Bulkhead) (metric.Registration, error) {
	m := policyMetrics{meter: meter}
	calls := m.counter("bulkhead.calls", "Calls that the bulkhead permitted or rejected, by result")
	waited := m.counter("bulkhead.waited_calls", "Calls that waited for a slot")
	inFlight := m.gauge("bulkhead.in_flight", "Calls in progress")
	waiting := m.gauge("bulkhead.waiting", "Callers that wait for a slot")
	return m.register(name, func(record recordFunc) {
		stats := b.Stats()
		record(calls, stats.Permitted, resultKey.String("permitted"))
		record(calls, stats.Rejected, resultKey.String("rejected"))
		record(waited, stats.Waited)
		record(inFlight, int64(stats.InFlight))
		record(waiting, int64(stats.Waiting))
	})
}

func RegisterConcurrencyLimitMetrics(meter metric.Meter, name string, l *concurrencylimit.Limiter) (metric.Registration, error) {
	m := policyMetrics{meter: meter}
	calls := m.counter("concurrency_limit.calls", "Calls that the concurrency limiter permitted or rejected, by result")
	decreases := m.counter("concurrency_limit.limit_decreases", "Overload signals that decreased the limit")
	limit := m.gauge("concurrency_limit.limit", "Current limit of calls in progress")
	inFlight := m.gauge("concurrency_limit.in_flight", "Calls in progress")
	return m.register(name, func(record recordFunc) {
		stats := l.Stats()
		record(calls, stats.Permitted, resultKey.String("permitted"))
		record(calls, stats.Rejected, resultKey.String("rejected"))
		record(decreases, stats.Decreases)
		record(limit, int64(stats.Limit))
		record(inFlight, int64(stats.InFlight))
	})
}

func RegisterLoadShedMetrics(meter metric.Meter, name string, s *loadshed.Shedder) (metric.Registration, error) {
	m := policyMetrics{meter: meter}
	calls := m.counter("load_shed.calls", "Calls that the load shedder admitted or shed, by result and priority")
	inFlight := m.gauge("load_shed.in_flight", "Calls in progress")
	return m.register(name, func(record recordFunc) {
		stats := s.Stats()
		for p, label := range priorityLabels {
			priority := priorityKey.String(label)
			record(calls, stats.Admitted[p], resultKey.String("admitted"), priority)
			record(calls, stats.Shed[p], resultKey.String("shed"), priority)
		}
		record(inFlight, int64(stats.InFlight))
	})
}
