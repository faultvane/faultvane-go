package faultvaneotel

import (
	"context"

	"github.com/faultvane/faultvane-go/ratelimit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const rateLimitPrefix = "faultvane.rate_limiter."

var (
	policyNameKey        = attribute.Key("faultvane.policy.name")
	rateLimitModeKey     = attribute.Key("faultvane.rate_limiter.mode")
	rateLimitDecisionKey = attribute.Key("faultvane.rate_limiter.decision")
	rateLimitModes       = []ratelimit.Mode{ratelimit.Distributed, ratelimit.Local}
)

func RegisterRateLimitMetrics(meter metric.Meter, name string, l *ratelimit.Limiter) (metric.Registration, error) {
	var failure error
	counter := func(name, description string) metric.Int64ObservableCounter {
		c, err := meter.Int64ObservableCounter(rateLimitPrefix+name, metric.WithDescription(description))
		if err != nil {
			failure = err
		}
		return c
	}
	gauge := func(name, description string) metric.Int64ObservableGauge {
		g, err := meter.Int64ObservableGauge(rateLimitPrefix+name, metric.WithDescription(description))
		if err != nil {
			failure = err
		}
		return g
	}
	calls := counter("calls", "Calls that the rate limiter decided, by decision")
	storeErrors := counter("store_errors", "Synchronizations with the store that failed")
	mode := gauge("mode", "1 for the current mode of the rate limiter, 0 for the other mode")
	allowance := gauge("allowance", "Calls that the rate limiter can permit now from local memory")
	if failure != nil {
		return nil, failure
	}
	policy := policyNameKey.String(name)
	return meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := l.Stats()
		observer.ObserveInt64(calls, stats.Permitted, metric.WithAttributes(policy, rateLimitDecisionKey.String("permitted")))
		observer.ObserveInt64(calls, stats.Rejected, metric.WithAttributes(policy, rateLimitDecisionKey.String("rejected")))
		observer.ObserveInt64(storeErrors, stats.StoreErrors, metric.WithAttributes(policy))
		for _, m := range rateLimitModes {
			observer.ObserveInt64(mode, flag(stats.Mode == m), metric.WithAttributes(policy, rateLimitModeKey.String(m.String())))
		}
		observer.ObserveInt64(allowance, stats.Allowance, metric.WithAttributes(policy))
		return nil
	}, calls, storeErrors, mode, allowance)
}
