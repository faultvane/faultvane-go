package redisstore

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/redis/go-redis/v9"
)

type countingStore struct {
	circuitbreaker.Store
	operations *atomic.Int64
}

func (s countingStore) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Load(ctx, name)
}

func (s countingStore) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Report(ctx, name, report)
}

type measurement struct {
	calls          int
	p50, p99, p999 time.Duration
	storeRate      float64
}

func measure(t *testing.T, instances int, scenario string, pause time.Duration) measurement {
	t.Helper()
	const duration = 1500 * time.Millisecond
	p := newProxy(t, standalone())
	client := redis.NewClient(&redis.Options{Addr: p.listener.Addr().String(), DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond, MaxRetries: -1, PoolSize: 20})
	defer func() { _ = client.Close() }()
	base := newStore(t, client)
	var operations atomic.Int64
	store := countingStore{Store: base, operations: &operations}
	breakers := make([]*circuitbreaker.Breaker, instances)
	for i := range breakers {
		breaker, err := circuitbreaker.New("dependency",
			circuitbreaker.WithStore(store), circuitbreaker.WithLogger(quiet),
			circuitbreaker.WithStoreTimeout(80*time.Millisecond), circuitbreaker.WithStartTimeout(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		breakers[i] = breaker
	}
	if scenario != "healthy" {
		p.stop()
	}
	operations.Store(0)
	samples := make([][]time.Duration, instances)
	deadline := time.Now().Add(duration)
	var group sync.WaitGroup
	for i, breaker := range breakers {
		group.Add(1)
		go func() {
			defer group.Done()
			call := func(context.Context) error { return nil }
			for time.Now().Before(deadline) {
				started := time.Now()
				_ = breaker.Do(context.Background(), call)
				samples[i] = append(samples[i], time.Since(started))
				if pause > 0 {
					time.Sleep(pause)
				} else if len(samples[i])%64 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	if scenario == "recovering" {
		time.Sleep(duration / 2)
		p.start()
	}
	group.Wait()
	rate := float64(operations.Load()) / duration.Seconds()
	if running := synchronizers(); running != instances {
		t.Errorf("PF-6 %d synchronizer goroutines run for %d instances", running, instances)
	}
	for _, breaker := range breakers {
		if err := breaker.Close(context.Background()); err != nil && scenario == "healthy" {
			t.Fatal(err)
		}
	}
	var all []time.Duration
	for _, s := range samples {
		all = append(all, s...)
	}
	slices.Sort(all)
	at := func(quantile float64) time.Duration { return all[int(float64(len(all)-1)*quantile)] }
	return measurement{calls: len(all), p50: at(0.5), p99: at(0.99), p999: at(0.999), storeRate: rate}
}

func synchronizers() int {
	buffer := make([]byte, 16<<20)
	return strings.Count(string(buffer[:runtime.Stack(buffer, true)]), "circuitbreaker.(*Breaker).run")
}

func TestSystem(t *testing.T) {
	if os.Getenv("FAULTVANE_SYSTEM_TEST") == "" {
		t.Skip("set FAULTVANE_SYSTEM_TEST=1 to run the system benchmark of acceptance test AT-7")
	}
	reachable(t, standalone())
	fmt.Println("| Instances | Store | Request rate | Calls | p50 | p99 | p99.9 | Store operations each second |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, instances := range []int{1, 10, 50, 100} {
		for _, scenario := range []string{"healthy", "unavailable", "recovering"} {
			t.Run(fmt.Sprintf("AT-7 PF-1 PF-3 %d instances with the store %s", instances, scenario), func(t *testing.T) {
				low := measure(t, instances, scenario, time.Millisecond)
				high := measure(t, instances, scenario, 0)
				for name, m := range map[string]measurement{"low": low, "high": high} {
					fmt.Printf("| %d | %s | %s | %d | %v | %v | %v | %.0f |\n", instances, scenario, name, m.calls, m.p50, m.p99, m.p999, m.storeRate)
					if m.p99 > time.Millisecond {
						t.Errorf("%s request rate: p99 = %v, want less than 1ms", name, m.p99)
					}
					if limit := float64(instances) * 3; m.storeRate > limit {
						t.Errorf("%s request rate: %.0f store operations each second, want at most %.0f", name, m.storeRate, limit)
					}
				}
				if high.calls < 10*low.calls {
					t.Errorf("the high rate made %d calls and the low rate %d, want 10 times more", high.calls, low.calls)
				}
				if running := synchronizers(); running != 0 {
					t.Errorf("PF-6 %d synchronizer goroutines run after Close", running)
				}
			})
		}
	}
}
