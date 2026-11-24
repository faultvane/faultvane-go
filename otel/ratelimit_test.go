package faultvaneotel

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/faultvanetest"
	"github.com/faultvane/faultvane-go/ratelimit"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type rateLimitDown struct{}

func (rateLimitDown) Acquire(context.Context, string, ratelimit.Acquire) (ratelimit.Grant, error) {
	return ratelimit.Grant{}, errDown
}

func rateLimitMetrics(t *testing.T, name string, opts ...ratelimit.Option) (*ratelimit.Limiter, *faultvanetest.Clock, func() map[string]int64) {
	t.Helper()
	clock := faultvanetest.NewClock(start)
	base := []ratelimit.Option{ratelimit.WithClock(clock), ratelimit.WithRate(10), ratelimit.WithBurst(5), ratelimit.WithSyncInterval(time.Hour)}
	limiter, err := ratelimit.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close(context.Background()) })
	reader := sdkmetric.NewManualReader()
	registration, err := RegisterRateLimitMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), name, limiter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Unregister() })
	return limiter, clock, func() map[string]int64 { return collect(t, reader) }
}

func rateLimitValues(name string, permitted, rejected, storeErrors, distributed, local, allowance int64) map[string]int64 {
	p := " faultvane.policy.name=" + name
	return map[string]int64{
		"faultvane.rate_limiter.calls" + p + " faultvane.rate_limiter.decision=permitted": permitted,
		"faultvane.rate_limiter.calls" + p + " faultvane.rate_limiter.decision=rejected":  rejected,
		"faultvane.rate_limiter.store_errors" + p:                                         storeErrors,
		"faultvane.rate_limiter.mode" + p + " faultvane.rate_limiter.mode=distributed":    distributed,
		"faultvane.rate_limiter.mode" + p + " faultvane.rate_limiter.mode=local":          local,
		"faultvane.rate_limiter.allowance" + p:                                            allowance,
	}
}

func TestRateLimitMetrics(t *testing.T) {
	assert := func(t *testing.T, got, want map[string]int64) {
		t.Helper()
		if !maps.Equal(got, want) {
			t.Fatalf("metrics = %v, want %v", got, want)
		}
	}

	t.Run("RL-36 RL-35 TK-40 TK-41 the metrics give the statistics of a limiter with a fixed set of labels", func(t *testing.T) {
		limiter, clock, metrics := rateLimitMetrics(t, "orders")
		assert(t, metrics(), rateLimitValues("orders", 0, 0, 0, 0, 1, 5))
		for range 7 {
			_ = limiter.Allow()
		}
		clock.Advance(250 * time.Millisecond)
		assert(t, metrics(), rateLimitValues("orders", 5, 2, 0, 0, 1, 2))
	})

	t.Run("RL-36 the metrics show distributed mode for a limiter with a store", func(t *testing.T) {
		store := ratelimit.NewMemoryStore(faultvanetest.NewClock(start))
		limiter, _, metrics := rateLimitMetrics(t, "payments", ratelimit.WithStore(store), ratelimit.WithName("payments"))
		_ = limiter.Allow()
		assert(t, metrics(), rateLimitValues("payments", 1, 0, 0, 1, 0, 4))
	})

	t.Run("RL-36 the metrics show local mode and the store errors", func(t *testing.T) {
		_, _, metrics := rateLimitMetrics(t, "payments", ratelimit.WithStore(rateLimitDown{}), ratelimit.WithName("payments"))
		assert(t, metrics(), rateLimitValues("payments", 0, 0, 1, 0, 1, 5))
	})

	t.Run("RL-36 a meter that cannot create an instrument gives an error", func(t *testing.T) {
		limiter, _, _ := rateLimitMetrics(t, "orders")
		for name, meter := range map[string]failing{"gauge": {failGauge: true}, "counter": {failCounter: true}} {
			if registration, err := RegisterRateLimitMetrics(meter, "orders", limiter); registration != nil || err != errInstrument {
				t.Errorf("%s: RegisterRateLimitMetrics() = %v, %v, want the error of the meter", name, registration, err)
			}
		}
	})
}
