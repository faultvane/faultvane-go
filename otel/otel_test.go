package faultvaneotel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/faultvanetest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	errDependency = errors.New("dependency failed")
	quiet         = slog.New(slog.DiscardHandler)
	start         = time.Unix(1_000_000, 0)
)

func newBreaker(t *testing.T, name string, opts ...circuitbreaker.Option) (*circuitbreaker.Breaker, *faultvanetest.Clock) {
	t.Helper()
	clock := faultvanetest.NewClock(start)
	base := []circuitbreaker.Option{circuitbreaker.WithClock(clock), circuitbreaker.WithLogger(quiet), circuitbreaker.WithMinimumCalls(2), circuitbreaker.WithOpenDuration(time.Minute)}
	breaker, err := circuitbreaker.New(name, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return breaker, clock
}

func run(breaker faultvane.Policy, ctx context.Context, result error) error {
	return breaker.Do(ctx, func(context.Context) error { return result })
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	values := map[string]int64{}
	add := func(name string, points []metricdata.DataPoint[int64]) {
		for _, point := range points {
			key := name
			for _, pair := range point.Attributes.ToSlice() {
				key += fmt.Sprintf(" %s=%s", pair.Key, pair.Value.String())
			}
			values[key] = point.Value
		}
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch points := m.Data.(type) {
			case metricdata.Sum[int64]:
				if !points.IsMonotonic || points.Temporality != metricdata.CumulativeTemporality {
					t.Errorf("%s is not a cumulative counter", m.Name)
				}
				add(m.Name, points.DataPoints)
			case metricdata.Gauge[int64]:
				add(m.Name, points.DataPoints)
			case metricdata.Histogram[float64]:
				for _, point := range points.DataPoints {
					key := m.Name
					for _, pair := range point.Attributes.ToSlice() {
						key += fmt.Sprintf(" %s=%s", pair.Key, pair.Value.String())
					}
					values[key] = int64(point.Count)
				}
			}
		}
	}
	return values
}

func TestMetrics(t *testing.T) {
	t.Run("OB-1 OB-2 TK-40 TK-41 the metrics give the statistics of each breaker with a fixed set of labels", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
		payment, clock := newBreaker(t, "payment", circuitbreaker.WithSlowCallDuration(time.Second), circuitbreaker.WithSlowCallRateThreshold(1))
		idle, _ := newBreaker(t, "idle")
		registration, err := RegisterMetrics(meter, payment, idle)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		_ = run(payment, ctx, nil)
		_ = run(payment, ctx, context.Canceled)
		_ = payment.Do(ctx, func(context.Context) error {
			clock.Advance(time.Second)
			return errDependency
		})
		_ = run(payment, ctx, nil)
		clock.Advance(time.Minute)
		probe, _ := payment.Allow(ctx)
		_ = run(payment, ctx, nil)
		probe.Failure()

		got := collect(t, reader)
		p := " faultvane.breaker.name=payment"
		want := map[string]int64{
			"faultvane.circuit_breaker.state" + p + " faultvane.breaker.state=closed":                                  0,
			"faultvane.circuit_breaker.state" + p + " faultvane.breaker.state=open":                                    1,
			"faultvane.circuit_breaker.state" + p + " faultvane.breaker.state=half_open":                               0,
			"faultvane.circuit_breaker.mode faultvane.breaker.mode=distributed" + p:                                    0,
			"faultvane.circuit_breaker.mode faultvane.breaker.mode=local" + p:                                          1,
			"faultvane.circuit_breaker.local_mode_expired" + p:                                                         0,
			"faultvane.circuit_breaker.calls" + p + " result=success":                                                  1,
			"faultvane.circuit_breaker.calls" + p + " result=failure":                                                  2,
			"faultvane.circuit_breaker.calls" + p + " result=ignored":                                                  1,
			"faultvane.circuit_breaker.slow_calls" + p:                                                                 1,
			"faultvane.circuit_breaker.rejected_calls" + p + " faultvane.breaker.rejection_reason=open":                1,
			"faultvane.circuit_breaker.rejected_calls" + p + " faultvane.breaker.rejection_reason=probe_not_permitted": 1,
			"faultvane.circuit_breaker.probes" + p + " result=success":                                                 0,
			"faultvane.circuit_breaker.probes" + p + " result=failure":                                                 1,
			"faultvane.circuit_breaker.transitions" + p + " to=open":                                                   2,
			"faultvane.circuit_breaker.transitions" + p + " to=half_open":                                              1,
			"faultvane.circuit_breaker.transitions" + p + " to=closed":                                                 0,
			"faultvane.circuit_breaker.store_errors" + p:                                                               0,
			"faultvane.circuit_breaker.local_mode" + p:                                                                 0,
			"faultvane.circuit_breaker.reconciliations" + p:                                                            0,
		}
		for key, value := range want {
			if actual, found := got[key]; !found || actual != value {
				t.Errorf("%s = %d (found %v), want %d", key, actual, found, value)
			}
		}
		if len(got) != 2*len(want) {
			t.Errorf("%d series, want %d for 2 breakers", len(got), 2*len(want))
		}
		if got["faultvane.circuit_breaker.state faultvane.breaker.name=idle faultvane.breaker.state=closed"] != 1 {
			t.Error("the idle breaker is not closed in the metrics")
		}

		if err := registration.Unregister(); err != nil {
			t.Fatal(err)
		}
		if after := collect(t, reader); len(after) != 0 {
			t.Errorf("%d series after Unregister, want 0", len(after))
		}
	})

	t.Run("OB-1 the metrics show local mode, store errors, and the limit of local mode", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
		breaker, err := circuitbreaker.New("shared", circuitbreaker.WithLogger(quiet), circuitbreaker.WithStore(down{}),
			circuitbreaker.WithStartTimeout(time.Second), circuitbreaker.WithLocalFallback(false), circuitbreaker.WithLocalModeExpiredPolicy(circuitbreaker.RejectAll),
			circuitbreaker.WithStartPolicy(circuitbreaker.StartClosed), circuitbreaker.WithSyncInterval(time.Hour), circuitbreaker.WithWindowSize(100*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = breaker.Close(context.Background()) }()
		if _, err := RegisterMetrics(meter, breaker); err != nil {
			t.Fatal(err)
		}
		got := collect(t, reader)
		s := " faultvane.breaker.name=shared"
		for key, value := range map[string]int64{
			"faultvane.circuit_breaker.mode faultvane.breaker.mode=local" + s:       1,
			"faultvane.circuit_breaker.mode faultvane.breaker.mode=distributed" + s: 0,
			"faultvane.circuit_breaker.store_errors" + s:                            1,
			"faultvane.circuit_breaker.local_mode" + s:                              1,
		} {
			if got[key] != value {
				t.Errorf("%s = %d, want %d", key, got[key], value)
			}
		}
	})

	t.Run("OB-1 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		if registration, err := RegisterMetrics(failing{failGauge: true}); registration != nil || err == nil {
			t.Fatalf("RegisterMetrics() = %v, %v, want an error", registration, err)
		}
		if registration, err := RegisterMetrics(failing{failCounter: true}); registration != nil || err == nil {
			t.Fatalf("RegisterMetrics() = %v, %v, want an error", registration, err)
		}
		if store, err := InstrumentStore(down{}, failing{}); store != nil || err == nil {
			t.Fatalf("InstrumentStore() = %v, %v, want an error", store, err)
		}
	})
}

type down struct{}

var errDown = fmt.Errorf("down: %w", faultvane.ErrStoreUnavailable)

func (down) Load(context.Context, string) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (down) Report(context.Context, string, circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (down) AcquireProbe(context.Context, string, circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	return circuitbreaker.ProbeGrant{}, errDown
}

func (down) CompleteProbe(context.Context, string, circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (down) Restore(context.Context, string, circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

type failing struct {
	metric.Meter
	failGauge   bool
	failCounter bool
}

var errInstrument = errors.New("instrument not available")

func (f failing) Int64ObservableGauge(string, ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	if f.failGauge {
		return nil, errInstrument
	}
	return nil, nil
}

func (f failing) Int64ObservableCounter(string, ...metric.Int64ObservableCounterOption) (metric.Int64ObservableCounter, error) {
	if f.failCounter {
		return nil, errInstrument
	}
	return nil, nil
}

func (failing) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errInstrument
}

func TestTraced(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)).Tracer("test")

	attributesOf := func(t *testing.T, policy faultvane.Policy, result error) (map[attribute.Key]string, error) {
		t.Helper()
		ctx, span := tracer.Start(context.Background(), "request")
		err := run(policy, ctx, result)
		span.End()
		ended := recorder.Ended()
		got := map[attribute.Key]string{}
		for _, pair := range ended[len(ended)-1].Attributes() {
			got[pair.Key] = pair.Value.String()
		}
		return got, err
	}
	expect := func(t *testing.T, got map[attribute.Key]string, want map[attribute.Key]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("attributes = %v, want %v", got, want)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("attributes = %v, want %v", got, want)
			}
		}
	}

	t.Run("OB-5 TK-44 the span gets the decision of a permitted call", func(t *testing.T) {
		breaker, _ := newBreaker(t, "payment")
		got, err := attributesOf(t, Traced(breaker), errDependency)
		if err != errDependency {
			t.Fatalf("Do() = %v, want the error of the call", err)
		}
		expect(t, got, map[attribute.Key]string{nameKey: "payment", stateKey: "closed", modeKey: "local", decisionKey: "permitted"})
	})

	t.Run("OB-5 the span gets the reason of a rejection and the decision of a probe", func(t *testing.T) {
		breaker, clock := newBreaker(t, "payment")
		policy := Traced(breaker)
		_ = run(policy, context.Background(), errDependency)
		_ = run(policy, context.Background(), errDependency)
		got, err := attributesOf(t, policy, nil)
		if err != circuitbreaker.ErrCircuitOpen {
			t.Fatalf("Do() = %v, want ErrCircuitOpen", err)
		}
		expect(t, got, map[attribute.Key]string{nameKey: "payment", stateKey: "open", modeKey: "local", decisionKey: "rejected", reasonKey: "open"})

		clock.Advance(time.Minute)
		held, allowErr := breaker.Allow(context.Background())
		if allowErr != nil {
			t.Fatal(allowErr)
		}
		got, err = attributesOf(t, policy, nil)
		if err != circuitbreaker.ErrProbeNotPermitted {
			t.Fatalf("Do() = %v, want ErrProbeNotPermitted", err)
		}
		expect(t, got, map[attribute.Key]string{nameKey: "payment", stateKey: "half_open", modeKey: "local", decisionKey: "rejected", reasonKey: "probe_not_permitted"})
		held.Ignore()

		got, err = attributesOf(t, policy, nil)
		if err != nil {
			t.Fatalf("Do() = %v, want nil for the probe", err)
		}
		expect(t, got, map[attribute.Key]string{nameKey: "payment", stateKey: "half_open", modeKey: "local", decisionKey: "probe"})
	})

	t.Run("OB-5 a context error from the breaker adds no decision", func(t *testing.T) {
		breaker, _ := newBreaker(t, "payment")
		ctx, span := tracer.Start(context.Background(), "request")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		err := run(Traced(breaker), cancelled, nil)
		span.End()
		ended := recorder.Ended()
		if err != context.Canceled || len(ended[len(ended)-1].Attributes()) != 3 {
			t.Fatalf("Do() = %v with %d attributes, want context.Canceled with 3", err, len(ended[len(ended)-1].Attributes()))
		}
	})

	t.Run("OB-5 TK-44 a call without a recording span runs the breaker directly", func(t *testing.T) {
		breaker, _ := newBreaker(t, "payment")
		policy := Traced(breaker)
		before := len(recorder.Ended())
		if err := run(policy, context.Background(), errDependency); err != errDependency {
			t.Fatalf("Do() = %v", err)
		}
		if breaker.Stats().Failures != 1 || len(recorder.Ended()) != before {
			t.Fatalf("failures = %d, spans = %d", breaker.Stats().Failures, len(recorder.Ended())-before)
		}
		ctx := context.Background()
		call := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = policy.Do(ctx, call) }); allocations != 0 {
			t.Fatalf("%v allocations for each call without a span, want 0", allocations)
		}
	})
}

func TestInstrumentStore(t *testing.T) {
	t.Run("OB-1 the store histogram counts each operation with its result", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
		ctx := context.Background()
		clock := faultvanetest.NewClock(start)
		memory := circuitbreaker.NewMemoryStore(clock)
		store, err := InstrumentStore(memory, meter)
		if err != nil {
			t.Fatal(err)
		}
		report := circuitbreaker.Report{Calls: 5, Failures: 5, Thresholds: circuitbreaker.Thresholds{FailureThreshold: 5, MinimumCalls: 100, FailureRate: 1}, WindowSize: time.Minute, WindowBuckets: 6, OpenDuration: time.Nanosecond}
		opened, _ := store.Report(ctx, "a", report)
		clock.Advance(time.Second)
		grant, _ := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 1, Limit: 1, LeaseDuration: time.Hour})
		closed, _ := store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Generation: 2, Token: 1, Result: circuitbreaker.Success, SuccessThreshold: 1})
		restored, _ := store.Restore(ctx, "a", circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 9})
		loaded, _ := store.Load(ctx, "a")
		states := []circuitbreaker.State{opened.State, grant.Snapshot.State, closed.State, restored.State, loaded.State}
		want := []circuitbreaker.State{circuitbreaker.Open, circuitbreaker.HalfOpen, circuitbreaker.Closed, circuitbreaker.Open, circuitbreaker.Open}
		if !grant.Granted || !slices.Equal(states, want) || loaded.Generation != 9 {
			t.Fatalf("granted = %v, states = %v, generation = %d", grant.Granted, states, loaded.Generation)
		}

		failed, err := InstrumentStore(down{}, meter)
		if err != nil {
			t.Fatal(err)
		}
		_, loadErr := failed.Load(ctx, "a")
		_, reportErr := failed.Report(ctx, "a", report)
		_, acquireErr := failed.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{})
		_, completeErr := failed.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{})
		_, restoreErr := failed.Restore(ctx, "a", circuitbreaker.Snapshot{})
		for _, err := range []error{loadErr, reportErr, acquireErr, completeErr, restoreErr} {
			if err != errDown {
				t.Fatalf("error = %v, want the error of the store without a change", err)
			}
		}

		got := collect(t, reader)
		for _, operation := range []string{"load", "report", "acquire_probe", "complete_probe", "restore"} {
			for _, failed := range []string{"false", "true"} {
				key := "faultvane.circuit_breaker.store.duration error=" + failed + " operation=" + operation
				if got[key] != 1 {
					t.Errorf("%s has %d measurements, want 1", key, got[key])
				}
			}
		}
		if len(got) != 10 {
			t.Errorf("%d series, want 10", len(got))
		}
	})
}
