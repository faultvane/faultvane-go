package concurrencylimit_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/concurrencylimit"
)

var errTooManyRequests = errors.New("search: too many requests")

func search(context.Context) error { return nil }

func Example() {
	limiter, err := concurrencylimit.New(
		concurrencylimit.WithInitialLimit(20),
		concurrencylimit.WithMinLimit(5),
		concurrencylimit.WithMaxLimit(100),
		concurrencylimit.WithLatencyThreshold(200*time.Millisecond),
		concurrencylimit.WithBackoffRatio(0.8),
	)
	if err != nil {
		panic(err)
	}

	err = limiter.Do(context.Background(), search)
	switch {
	case errors.Is(err, concurrencylimit.ErrLimitExceeded):
		fmt.Println("the search service is at its limit")
	case errors.Is(err, faultvane.ErrRejected):
		fmt.Println("a policy rejected the call")
	case err != nil:
		fmt.Println("the call failed:", err)
	}
}

func ExampleWithOverloadIf() {
	limiter, err := concurrencylimit.New(
		concurrencylimit.WithOverloadIf(func(err error) bool {
			return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errTooManyRequests)
		}),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(limiter.Limit(), limiter.InFlight())
}

type limitPrinter struct{}

func (limitPrinter) Observe(event concurrencylimit.Event) {
	fmt.Println(event.Kind, event.From, event.To)
}

func ExampleWithObserver() {
	limiter, err := concurrencylimit.New(
		concurrencylimit.WithObserver(limitPrinter{}),
		concurrencylimit.WithLogger(slog.Default().With("limiter", "search")),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(limiter.Limit())
}
