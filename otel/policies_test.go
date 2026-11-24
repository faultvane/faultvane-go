package faultvaneotel

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/concurrencylimit"
	"github.com/faultvane/faultvane-go/loadshed"
	"github.com/faultvane/faultvane-go/retry"
	"github.com/faultvane/faultvane-go/timeout"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func succeed(context.Context) error { return nil }

func fail(context.Context) error { return errDependency }

func must[T any](t *testing.T) func(T, error) T {
	return func(value T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
}

func newMeter() (*sdkmetric.ManualReader, metric.Meter) {
	reader := sdkmetric.NewManualReader()
	return reader, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
}

func expectSeries(t *testing.T, reader *sdkmetric.ManualReader, want map[string]int64) {
	t.Helper()
	got := collect(t, reader)
	for key, value := range want {
		if actual, found := got[key]; !found || actual != value {
			t.Errorf("%s = %d (found %v), want %d", key, actual, found, value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d series %v, want %d", len(got), got, len(want))
	}
}

func expectUnregistered(t *testing.T, reader *sdkmetric.ManualReader, registration metric.Registration) {
	t.Helper()
	if err := registration.Unregister(); err != nil {
		t.Fatal(err)
	}
	if after := collect(t, reader); len(after) != 0 {
		t.Errorf("%d series after Unregister, want 0", len(after))
	}
}

func expectNoRegistration(t *testing.T, registration metric.Registration, err error) {
	t.Helper()
	if registration != nil || err != errInstrument {
		t.Fatalf("registration = %v, %v, want nil and the error of the meter", registration, err)
	}
}

func TestTimeoutMetrics(t *testing.T) {
	t.Run("TO-17 TK-40 TK-41 the metrics give the calls and the timeouts", func(t *testing.T) {
		reader, meter := newMeter()
		policy := must[*timeout.Timeout](t)(timeout.New(time.Nanosecond))
		registration := must[metric.Registration](t)(RegisterTimeoutMetrics(meter, "payment", policy))
		ctx := context.Background()
		_ = policy.Do(ctx, succeed)
		for range 2 {
			_ = policy.Do(ctx, func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
		}
		expectSeries(t, reader, map[string]int64{
			"faultvane.timeout.calls faultvane.policy.name=payment":    3,
			"faultvane.timeout.timeouts faultvane.policy.name=payment": 2,
		})
		expectUnregistered(t, reader, registration)
	})

	t.Run("TO-17 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		registration, err := RegisterTimeoutMetrics(failing{failCounter: true}, "payment", nil)
		expectNoRegistration(t, registration, err)
	})
}

func TestRetryMetrics(t *testing.T) {
	t.Run("RT-22 TK-40 TK-41 the metrics give the calls, the retries, and the stops", func(t *testing.T) {
		reader, meter := newMeter()
		open := true
		policy := must[*retry.Retry](t)(retry.New(retry.WithBackoff(time.Nanosecond, time.Nanosecond), retry.WithBudget(0.001), retry.WithGate(func() bool { return open })))
		registration := must[metric.Registration](t)(RegisterRetryMetrics(meter, "payment", policy))
		ctx := context.Background()
		_ = policy.Do(ctx, succeed)
		attempts := 0
		_ = policy.Do(ctx, func(context.Context) error {
			attempts++
			if attempts == 1 {
				return errDependency
			}
			return nil
		})
		_ = policy.Do(ctx, fail)
		open = false
		_ = policy.Do(ctx, fail)
		_ = policy.Do(ctx, fail)
		open = true
		for range 4 {
			_ = policy.Do(ctx, fail)
		}
		expectSeries(t, reader, map[string]int64{
			"faultvane.retry.calls faultvane.policy.name=payment":        9,
			"faultvane.retry.retries faultvane.policy.name=payment":      10,
			"faultvane.retry.failed_calls faultvane.policy.name=payment": 7,
			"faultvane.retry.gate_stops faultvane.policy.name=payment":   2,
			"faultvane.retry.budget_stops faultvane.policy.name=payment": 1,
		})
		expectUnregistered(t, reader, registration)
	})

	t.Run("RT-22 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		registration, err := RegisterRetryMetrics(failing{failCounter: true}, "payment", nil)
		expectNoRegistration(t, registration, err)
	})
}

func TestBulkheadMetrics(t *testing.T) {
	t.Run("BH-24 TK-40 TK-41 the metrics give the decisions and the current load of each bulkhead", func(t *testing.T) {
		reader, meter := newMeter()
		payments := must[*bulkhead.Bulkhead](t)(bulkhead.New(bulkhead.WithMaxConcurrent(1), bulkhead.WithMaxWaiting(2)))
		search := must[*bulkhead.Bulkhead](t)(bulkhead.New())
		registration := must[metric.Registration](t)(RegisterBulkheadMetrics(meter, "payments", payments))
		idle := must[metric.Registration](t)(RegisterBulkheadMetrics(meter, "search", search))
		ctx := context.Background()
		p, s := " faultvane.policy.name=payments", " faultvane.policy.name=search"
		waiters := make(chan error, 2)
		_ = payments.Do(ctx, func(context.Context) error {
			for range cap(waiters) {
				go func() { waiters <- payments.Do(ctx, succeed) }()
			}
			for deadline := time.Now().Add(10 * time.Second); payments.Waiting() != cap(waiters); runtime.Gosched() {
				if time.Now().After(deadline) {
					t.Fatal("the callers do not wait")
				}
			}
			if err := payments.Do(ctx, succeed); err != bulkhead.ErrFull {
				t.Fatalf("Do() = %v, want ErrFull", err)
			}
			expectSeries(t, reader, map[string]int64{
				"faultvane.bulkhead.calls" + p + " result=permitted": 1,
				"faultvane.bulkhead.calls" + p + " result=rejected":  1,
				"faultvane.bulkhead.waited_calls" + p:                2,
				"faultvane.bulkhead.in_flight" + p:                   1,
				"faultvane.bulkhead.waiting" + p:                     2,
				"faultvane.bulkhead.calls" + s + " result=permitted": 0,
				"faultvane.bulkhead.calls" + s + " result=rejected":  0,
				"faultvane.bulkhead.waited_calls" + s:                0,
				"faultvane.bulkhead.in_flight" + s:                   0,
				"faultvane.bulkhead.waiting" + s:                     0,
			})
			return nil
		})
		for range cap(waiters) {
			if err := <-waiters; err != nil {
				t.Fatalf("Do() of a waiting caller = %v, want nil", err)
			}
		}
		if err := idle.Unregister(); err != nil {
			t.Fatal(err)
		}
		expectSeries(t, reader, map[string]int64{
			"faultvane.bulkhead.calls" + p + " result=permitted": 3,
			"faultvane.bulkhead.calls" + p + " result=rejected":  1,
			"faultvane.bulkhead.waited_calls" + p:                2,
			"faultvane.bulkhead.in_flight" + p:                   0,
			"faultvane.bulkhead.waiting" + p:                     0,
		})
		expectUnregistered(t, reader, registration)
	})

	t.Run("BH-24 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		for _, meter := range []failing{{failCounter: true}, {failGauge: true}} {
			registration, err := RegisterBulkheadMetrics(meter, "payments", nil)
			expectNoRegistration(t, registration, err)
		}
	})
}

func TestConcurrencyLimitMetrics(t *testing.T) {
	t.Run("CL-25 TK-40 TK-41 the metrics give the decisions, the limit, and the calls in progress", func(t *testing.T) {
		reader, meter := newMeter()
		limiter := must[*concurrencylimit.Limiter](t)(concurrencylimit.New(concurrencylimit.WithInitialLimit(2), concurrencylimit.WithBackoffRatio(0.5)))
		registration := must[metric.Registration](t)(RegisterConcurrencyLimitMetrics(meter, "payment", limiter))
		ctx := context.Background()
		p := " faultvane.policy.name=payment"
		_ = limiter.Do(ctx, func(context.Context) error {
			return limiter.Do(ctx, func(context.Context) error {
				if err := limiter.Do(ctx, succeed); err != concurrencylimit.ErrLimitExceeded {
					t.Fatalf("Do() = %v, want ErrLimitExceeded", err)
				}
				expectSeries(t, reader, map[string]int64{
					"faultvane.concurrency_limit.calls" + p + " result=permitted": 2,
					"faultvane.concurrency_limit.calls" + p + " result=rejected":  1,
					"faultvane.concurrency_limit.limit_decreases" + p:             0,
					"faultvane.concurrency_limit.limit" + p:                       2,
					"faultvane.concurrency_limit.in_flight" + p:                   2,
				})
				return nil
			})
		})
		_ = limiter.Do(ctx, func(context.Context) error { return context.DeadlineExceeded })
		expectSeries(t, reader, map[string]int64{
			"faultvane.concurrency_limit.calls" + p + " result=permitted": 3,
			"faultvane.concurrency_limit.calls" + p + " result=rejected":  1,
			"faultvane.concurrency_limit.limit_decreases" + p:             1,
			"faultvane.concurrency_limit.limit" + p:                       1,
			"faultvane.concurrency_limit.in_flight" + p:                   0,
		})
		expectUnregistered(t, reader, registration)
	})

	t.Run("CL-25 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		for _, meter := range []failing{{failCounter: true}, {failGauge: true}} {
			registration, err := RegisterConcurrencyLimitMetrics(meter, "payment", nil)
			expectNoRegistration(t, registration, err)
		}
	})
}

func TestLoadShedMetrics(t *testing.T) {
	t.Run("LS-22 TK-40 TK-41 the metrics give the decisions of each priority and the calls in progress", func(t *testing.T) {
		reader, meter := newMeter()
		shedder := must[*loadshed.Shedder](t)(loadshed.New(loadshed.WithMaxInFlight(2)))
		registration := must[metric.Registration](t)(RegisterLoadShedMetrics(meter, "api", shedder))
		with := func(p loadshed.Priority) context.Context {
			return loadshed.WithPriority(context.Background(), p)
		}
		series := func(priority, result string) string {
			return "faultvane.load_shed.calls faultvane.policy.name=api priority=" + priority + " result=" + result
		}
		_ = shedder.Do(with(loadshed.Low), succeed)
		_ = shedder.Do(with(loadshed.Critical), func(context.Context) error {
			_ = shedder.Do(with(loadshed.Low), succeed)
			_ = shedder.Do(with(loadshed.High), succeed)
			return shedder.Do(with(loadshed.Critical), func(context.Context) error {
				_ = shedder.Do(with(loadshed.Critical), succeed)
				_ = shedder.Do(context.Background(), succeed)
				expectSeries(t, reader, map[string]int64{
					series("critical", "admitted"):                            2,
					series("high", "admitted"):                                1,
					series("normal", "admitted"):                              0,
					series("low", "admitted"):                                 1,
					series("critical", "shed"):                                1,
					series("high", "shed"):                                    0,
					series("normal", "shed"):                                  1,
					series("low", "shed"):                                     1,
					"faultvane.load_shed.in_flight faultvane.policy.name=api": 2,
				})
				return nil
			})
		})
		expectUnregistered(t, reader, registration)
	})

	t.Run("LS-22 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		for _, meter := range []failing{{failCounter: true}, {failGauge: true}} {
			registration, err := RegisterLoadShedMetrics(meter, "api", nil)
			expectNoRegistration(t, registration, err)
		}
	})
}
