package faultvane_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/concurrencylimit"
	"github.com/faultvane/faultvane-go/faultvanetest"
	"github.com/faultvane/faultvane-go/loadshed"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/retry"
	"github.com/faultvane/faultvane-go/timeout"
)

var errOutage = errors.New("dependency failed")

func must[T any](t *testing.T) func(T, error) T {
	return func(value T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
}

func newBreaker(t *testing.T, opts ...circuitbreaker.Option) *circuitbreaker.Breaker {
	t.Helper()
	base := []circuitbreaker.Option{
		circuitbreaker.WithLogger(slog.New(slog.DiscardHandler)),
		circuitbreaker.WithClock(faultvanetest.NewClock(time.Unix(1_000_000, 0))),
		circuitbreaker.WithMinimumCalls(4),
	}
	return must[*circuitbreaker.Breaker](t)(circuitbreaker.New("dependency", append(base, opts...)...))
}

func TestPolicyPipeline(t *testing.T) {
	ctx := context.Background()

	t.Run("PI-1 TK-3 a call goes through all policies in the recommended order", func(t *testing.T) {
		breaker := newBreaker(t)
		pipeline := faultvane.Combine(
			must[*loadshed.Shedder](t)(loadshed.New()),
			must[*ratelimit.Limiter](t)(ratelimit.New()),
			must[*concurrencylimit.Limiter](t)(concurrencylimit.New()),
			must[*bulkhead.Bulkhead](t)(bulkhead.New()),
			breaker,
			must[*retry.Retry](t)(retry.New(retry.WithBackoff(time.Nanosecond, time.Nanosecond))),
			must[*timeout.Timeout](t)(timeout.New(time.Hour)),
		)
		attempts := 0
		value, err := faultvane.Execute(ctx, pipeline, func(ctx context.Context) (string, error) {
			attempts++
			if _, limited := ctx.Deadline(); !limited {
				t.Error("the call has no deadline from the timeout policy")
			}
			if attempts < 3 {
				return "", errOutage
			}
			return "receipt", nil
		})
		if value != "receipt" || err != nil || attempts != 3 {
			t.Fatalf("Execute() = %q, %v after %d attempts, want receipt, nil, 3", value, err, attempts)
		}
		if stats := breaker.Stats(); stats.Successes != 1 || stats.Failures != 0 {
			t.Fatalf("the breaker counted %d successes and %d failures, want 1 and 0", stats.Successes, stats.Failures)
		}
	})

	t.Run("PI-3 a retry with a breaker gate does not multiply the traffic during an outage", func(t *testing.T) {
		breaker := newBreaker(t)
		retrier := must[*retry.Retry](t)(retry.New(
			retry.WithMaxAttempts(5),
			retry.WithBackoff(time.Nanosecond, time.Nanosecond),
			retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
		))
		pipeline := faultvane.Combine(breaker, retrier)
		attempts, rejected := 0, 0
		for range 100 {
			err := pipeline.Do(ctx, func(context.Context) error {
				attempts++
				return errOutage
			})
			if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
				rejected++
			}
		}
		if attempts != 20 || rejected != 96 {
			t.Fatalf("the dependency got %d attempts and %d requests were rejected, want 20 and 96", attempts, rejected)
		}
	})

	t.Run("PI-3 a retry inside an open breaker stops at the gate", func(t *testing.T) {
		breaker := newBreaker(t)
		retrier := must[*retry.Retry](t)(retry.New(
			retry.WithMaxAttempts(10),
			retry.WithBackoff(time.Nanosecond, time.Nanosecond),
			retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
		))
		attempts := 0
		err := faultvane.Combine(retrier, breaker).Do(ctx, func(context.Context) error {
			attempts++
			return errOutage
		})
		if err != errOutage || attempts != 4 || breaker.State() != circuitbreaker.Open {
			t.Fatalf("err = %v after %d attempts in %v, want the error of the call after 4 attempts in open", err, attempts, breaker.State())
		}
	})

	t.Run("PI-3 a retry budget limits the extra traffic to its ratio", func(t *testing.T) {
		retrier := must[*retry.Retry](t)(retry.New(retry.WithBackoff(time.Nanosecond, time.Nanosecond), retry.WithBudget(0.1)))
		attempts := 0
		for range 1000 {
			_ = retrier.Do(ctx, func(context.Context) error {
				attempts++
				return errOutage
			})
		}
		if attempts < 1100 || attempts > 1110 {
			t.Fatalf("1000 failed requests made %d attempts, want from 1100 to 1110", attempts)
		}
	})

	t.Run("PI-1 a rejection of an outer policy is not a failure of the dependency", func(t *testing.T) {
		breaker := newBreaker(t)
		limiter := must[*ratelimit.Limiter](t)(ratelimit.New(ratelimit.WithRate(1), ratelimit.WithBurst(1), ratelimit.WithClock(faultvanetest.NewClock(time.Unix(0, 0)))))
		pipeline := faultvane.Combine(limiter, breaker)
		calls := 0
		var last error
		for range 50 {
			last = pipeline.Do(ctx, func(context.Context) error {
				calls++
				return nil
			})
		}
		if calls != 1 || !errors.Is(last, ratelimit.ErrRateLimited) || !errors.Is(last, faultvane.ErrRejected) {
			t.Fatalf("calls = %d, last error = %v, want 1 call and ErrRateLimited", calls, last)
		}
		if stats := breaker.Stats(); stats.Failures != 0 || stats.Successes != 1 || breaker.State() != circuitbreaker.Closed {
			t.Fatalf("breaker statistics = %+v", stats)
		}
	})

	t.Run("PI-1 a timeout inside a breaker is a failure for the breaker", func(t *testing.T) {
		breaker := newBreaker(t, circuitbreaker.WithMinimumCalls(1))
		pipeline := faultvane.Combine(breaker, must[*timeout.Timeout](t)(timeout.New(time.Millisecond)))
		err := pipeline.Do(ctx, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, timeout.ErrTimeout) || errors.Is(err, faultvane.ErrRejected) || breaker.State() != circuitbreaker.Open {
			t.Fatalf("err = %v in %v, want ErrTimeout and an open breaker", err, breaker.State())
		}
	})

	t.Run("PI-1 load shedding rejects low-priority work before it reaches the other policies", func(t *testing.T) {
		shedder := must[*loadshed.Shedder](t)(loadshed.New(loadshed.WithMaxInFlight(2)))
		breaker := newBreaker(t)
		pipeline := faultvane.Combine(shedder, breaker)
		var low, critical error
		_ = pipeline.Do(ctx, func(context.Context) error {
			low = pipeline.Do(loadshed.WithPriority(ctx, loadshed.Low), func(context.Context) error { return nil })
			critical = pipeline.Do(loadshed.WithPriority(ctx, loadshed.Critical), func(context.Context) error { return nil })
			return nil
		})
		if !errors.Is(low, loadshed.ErrShed) || critical != nil || breaker.Stats().Successes != 2 {
			t.Fatalf("low = %v, critical = %v, successes = %d", low, critical, breaker.Stats().Successes)
		}
	})
}
