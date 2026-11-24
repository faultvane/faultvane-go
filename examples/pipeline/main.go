package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/retry"
	"github.com/faultvane/faultvane-go/timeout"
)

func main() {
	if err := run(os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out io.Writer) error {
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, "receipt")
	}))
	defer server.Close()

	breaker, err := circuitbreaker.New("payment-service",
		circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		circuitbreaker.WithMinimumCalls(4),
		circuitbreaker.WithOpenDuration(200*time.Millisecond),
	)
	if err != nil {
		return err
	}
	limit, err := bulkhead.New(bulkhead.WithMaxConcurrent(10))
	if err != nil {
		return err
	}
	retrier, err := retry.New(
		retry.WithMaxAttempts(2),
		retry.WithBackoff(time.Millisecond, 10*time.Millisecond),
		retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
	)
	if err != nil {
		return err
	}
	limited, err := timeout.New(time.Second)
	if err != nil {
		return err
	}
	pipeline := faultvane.Combine(limit, breaker, retrier, limited)

	charge := func(ctx context.Context) (string, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			return "", err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return "", err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("payment service: status %d", response.StatusCode)
		}
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	request := func(label string) {
		value, err := faultvane.Execute(context.Background(), pipeline, charge)
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(out, "%s: %s (circuit %s)\n", label, value, breaker.State())
		case errors.Is(err, faultvane.ErrRejected):
			_, _ = fmt.Fprintf(out, "%s: rejected without a call (circuit %s)\n", label, breaker.State())
		default:
			_, _ = fmt.Fprintf(out, "%s: failed (circuit %s)\n", label, breaker.State())
		}
	}

	request("healthy dependency")
	healthy.Store(false)
	for i := 1; i <= 4; i++ {
		request(fmt.Sprintf("outage, request %d", i))
	}
	healthy.Store(true)
	request("dependency recovered, open duration not ended")
	time.Sleep(250 * time.Millisecond)
	request("after the open duration")
	request("normal operation")
	return nil
}
