package faultvane_test

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/concurrencylimit"
	"github.com/faultvane/faultvane-go/loadshed"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/retry"
	"github.com/faultvane/faultvane-go/timeout"
)

const (
	longRunWorkers     = 4
	longRunSamples     = 20
	longRunHeapMargin  = 1 << 20
	longRunOutageEvery = 10 * time.Second
	longRunOutageTime  = 2 * time.Second
)

type usage struct {
	heap       uint64
	goroutines int
}

func measure() usage {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return usage{heap: stats.HeapAlloc, goroutines: runtime.NumGoroutine()}
}

func TestLongRun(t *testing.T) {
	duration, err := time.ParseDuration(os.Getenv("FAULTVANE_LONG_RUN"))
	if err != nil {
		t.Skip("set FAULTVANE_LONG_RUN to a duration, for example 1h")
	}
	clock := faultvane.SystemClock()
	breaker := must[*circuitbreaker.Breaker](t)(circuitbreaker.New("dependency",
		circuitbreaker.WithLogger(slog.New(slog.DiscardHandler)),
		circuitbreaker.WithStore(circuitbreaker.NewMemoryStore(clock)),
		circuitbreaker.WithMinimumCalls(20),
		circuitbreaker.WithOpenDuration(500*time.Millisecond),
	))
	limiter := must[*ratelimit.Limiter](t)(ratelimit.New(
		ratelimit.WithRate(1_000_000),
		ratelimit.WithStore(ratelimit.NewMemoryStore(clock)),
		ratelimit.WithName("dependency"),
	))
	pipeline := faultvane.Combine(
		must[*loadshed.Shedder](t)(loadshed.New()),
		limiter,
		must[*concurrencylimit.Limiter](t)(concurrencylimit.New()),
		must[*bulkhead.Bulkhead](t)(bulkhead.New()),
		breaker,
		must[*retry.Retry](t)(retry.New(retry.WithBackoff(time.Microsecond, time.Millisecond), retry.WithBudget(0.1))),
		must[*timeout.Timeout](t)(timeout.New(time.Second)),
	)

	started := time.Now()
	dependency := func(context.Context) error {
		if time.Since(started)%longRunOutageEvery < longRunOutageTime {
			return errOutage
		}
		return nil
	}
	ctx, stop := context.WithTimeout(context.Background(), duration)
	defer stop()
	var workers sync.WaitGroup
	for range longRunWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ctx.Err() == nil {
				if pipeline.Do(ctx, dependency) != nil {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}()
	}

	var samples []usage
	for range longRunSamples {
		time.Sleep(duration / longRunSamples)
		samples = append(samples, measure())
		t.Logf("after %v: heap %d bytes, %d goroutines", time.Since(started).Round(time.Second), samples[len(samples)-1].heap, samples[len(samples)-1].goroutines)
	}
	workers.Wait()

	warm := longRunSamples / 4
	var limit usage
	for _, sample := range samples[:warm] {
		limit.heap, limit.goroutines = max(limit.heap, sample.heap), max(limit.goroutines, sample.goroutines)
	}
	for index, sample := range samples[warm:] {
		if sample.heap > limit.heap+longRunHeapMargin || sample.goroutines > limit.goroutines {
			t.Errorf("PF-5 PF-6 sample %d: heap %d bytes and %d goroutines, want at most %d bytes and %d goroutines", warm+index, sample.heap, sample.goroutines, limit.heap+longRunHeapMargin, limit.goroutines)
		}
	}
	stats := breaker.Stats()
	if duration >= 2*longRunOutageEvery && (stats.Opened == 0 || stats.Closed == 0 || stats.Successes == 0) {
		t.Errorf("the breaker opened %d times, closed %d times, and counted %d successes, want each above 0", stats.Opened, stats.Closed, stats.Successes)
	}
	if err := breaker.Close(context.Background()); err != nil {
		t.Error(err)
	}
	if err := limiter.Close(context.Background()); err != nil {
		t.Error(err)
	}
	t.Logf("%d successes, %d failures, opened %d times in %v", stats.Successes, stats.Failures, stats.Opened, duration)
}
