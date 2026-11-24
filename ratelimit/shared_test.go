package ratelimit

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/faultvanetest"
)

const sharedName = "orders"

var errStoreDown = errors.New("store down")

type recordingStore struct {
	Store
	down     atomic.Bool
	mu       sync.Mutex
	requests []Acquire
}

func (s *recordingStore) Acquire(ctx context.Context, name string, request Acquire) (Grant, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	if s.down.Load() {
		return Grant{}, errStoreDown
	}
	return s.Store.Acquire(ctx, name, request)
}

func (s *recordingStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *recordingStore) last() Acquire {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[len(s.requests)-1]
}

type sharedFixture struct {
	t     *testing.T
	clock *faultvanetest.Clock
	store *recordingStore
}

func newSharedFixture(t *testing.T) *sharedFixture {
	clock := faultvanetest.NewClock(testStart)
	return &sharedFixture{t: t, clock: clock, store: &recordingStore{Store: NewMemoryStore(clock)}}
}

func manualSync(c *config) { c.manualSync = true }

func (f *sharedFixture) limiter(opts ...Option) *Limiter {
	f.t.Helper()
	base := []Option{WithClock(f.clock), WithRate(100), WithBurst(50), WithStore(f.store), WithName(sharedName), manualSync}
	limiter, err := New(append(base, opts...)...)
	if err != nil {
		f.t.Fatal(err)
	}
	return limiter
}

func synchronize(limiter *Limiter) {
	limiter.synchronize(1)
}

func call(limiter *Limiter, times int) int {
	count := 0
	for range times {
		if limiter.Allow() {
			count++
		}
	}
	return count
}

func assertStats(t *testing.T, limiter *Limiter, want Stats) {
	t.Helper()
	if got := limiter.Stats(); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSharedBucket(t *testing.T) {
	t.Run("RL-20 RL-21 TK-31 limiters with one name permit the burst one time", func(t *testing.T) {
		f := newSharedFixture(t)
		first, second := f.limiter(), f.limiter()
		total := 0
		for range 4 {
			total += call(first, 100) + call(second, 100)
			synchronize(first)
			synchronize(second)
		}
		if total != 50 {
			t.Fatalf("permitted calls = %d, want the burst of 50", total)
		}
		f.clock.Advance(100 * time.Millisecond)
		call(first, 5)
		synchronize(first)
		synchronize(second)
		if total = call(first, 100) + call(second, 100); total != 10 {
			t.Fatalf("permitted calls after 100 ms = %d, want 10", total)
		}
	})

	t.Run("RL-20 limiters with different names share nothing", func(t *testing.T) {
		f := newSharedFixture(t)
		first, second := f.limiter(WithBurst(5)), f.limiter(WithBurst(5), WithName("payments"))
		if got := call(first, 10) + call(second, 10); got != 10 {
			t.Fatalf("permitted calls = %d, want 5 for each name", got)
		}
	})

	t.Run("RL-22 RL-23 TK-10 TK-35 a call makes no store call", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		before := f.store.calls()
		ctx := context.Background()
		for range 1000 {
			_ = limiter.Allow()
			_ = limiter.Do(ctx, func(context.Context) error { return nil })
		}
		if after := f.store.calls(); before != 1 || after != 1 {
			t.Fatalf("store calls = %d before and %d after 2000 calls, want 1 and 1", before, after)
		}
		synchronize(limiter)
		if got := f.store.calls(); got != 2 {
			t.Fatalf("store calls after one synchronization = %d, want 2", got)
		}
	})

	t.Run("RL-22 RL-4 a call without allowance is rejected while the store has tokens", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		assertPermitted(t, limiter, 10)
		err := limiter.Do(context.Background(), func(context.Context) error { return nil })
		if err != ErrRateLimited {
			t.Fatalf("Do() = %v, want ErrRateLimited", err)
		}
	})
}

func TestBatch(t *testing.T) {
	t.Run("RL-25 the first batch is the rate of one synchronization interval", func(t *testing.T) {
		cases := map[string]struct {
			opts []Option
			want int64
		}{
			"rate 100 and 100 ms":             {nil, 10},
			"rate 100 and 250 ms":             {[]Option{WithSyncInterval(250 * time.Millisecond)}, 25},
			"a part of a token is rounded up": {[]Option{WithRate(0.5)}, 1},
			"the burst is the maximum":        {[]Option{WithBurst(3)}, 3},
		}
		for name, tc := range cases {
			f := newSharedFixture(t)
			limiter := f.limiter(tc.opts...)
			if request := f.store.last(); request.Wanted != tc.want || request.Returned != 0 {
				t.Errorf("%s: first request = %+v, want %d tokens", name, request, tc.want)
			}
			if got := limiter.Stats().Allowance; got != tc.want {
				t.Errorf("%s: allowance = %d, want %d", name, got, tc.want)
			}
		}
	})

	t.Run("RL-24 the batch is two times the calls of the last interval", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		assertPermitted(t, limiter, 10)
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 22 || request.Returned != 0 {
			t.Fatalf("request after 10 permitted calls and 1 rejected call = %+v, want 22 tokens", request)
		}
		assertStats(t, limiter, Stats{Permitted: 10, Rejected: 1, Allowance: 22})
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 0 || request.Returned != 21 {
			t.Fatalf("request after an interval without calls = %+v, want 21 tokens back", request)
		}
	})

	t.Run("RL-24 the limiter asks only for the tokens that it does not hold", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		call(limiter, 8)
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 14 || request.Returned != 0 {
			t.Fatalf("request with 2 tokens and a batch of 16 = %+v, want 14 tokens", request)
		}
		assertStats(t, limiter, Stats{Permitted: 8, Allowance: 16})
	})

	t.Run("RL-24 the limiter gives back the tokens above the batch", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter, other := f.limiter(), f.limiter()
		call(limiter, 3)
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 0 || request.Returned != 1 {
			t.Fatalf("request with 7 tokens and a batch of 6 = %+v, want 1 token back", request)
		}
		assertStats(t, limiter, Stats{Permitted: 3, Allowance: 6})
		call(other, 1000)
		synchronize(other)
		if got := call(other, 1000); got != 31 {
			t.Fatalf("the other limiter permitted %d calls, want the 31 tokens of the store", got)
		}
	})

	t.Run("RL-24 an idle limiter keeps one token", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 0 || request.Returned != 9 {
			t.Fatalf("request of an idle limiter = %+v, want 9 tokens back", request)
		}
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 0 || request.Returned != 0 {
			t.Fatalf("second request of an idle limiter = %+v, want no tokens", request)
		}
		assertStats(t, limiter, Stats{Allowance: 1})
	})

	t.Run("RL-24 the batch is never above the burst", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		call(limiter, 1000)
		f.clock.Advance(time.Hour)
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 50 || request.Returned != 0 {
			t.Fatalf("request after 1000 calls = %+v, want the burst of 50", request)
		}
		assertStats(t, limiter, Stats{Permitted: 10, Rejected: 990, Allowance: 50})
	})

	t.Run("RL-24 each request carries the rate, the burst, and a time to live above the refill time", func(t *testing.T) {
		f := newSharedFixture(t)
		f.limiter(WithRate(0.5), WithBurst(60))
		want := Acquire{Wanted: 1, Rate: 0.5, Burst: 60, StateTTL: 3 * time.Minute}
		if got := f.store.last(); got != want {
			t.Fatalf("request = %+v, want %+v", got, want)
		}
		f.limiter(WithRate(1e-300), WithBurst(1))
		if got := f.store.last().StateTTL; got != time.Minute+maxWhole {
			t.Fatalf("time to live for a very slow rate = %v, want the maximum", got)
		}
	})
}

func TestGuarantee(t *testing.T) {
	const (
		rate     = 50.0
		burst    = 20
		interval = 100 * time.Millisecond
	)

	t.Run("RL-26 the permitted calls of all limiters stay inside the bound in each time window", func(t *testing.T) {
		type sample struct {
			at        time.Duration
			permitted int
			held      int64
		}
		for seed := range uint64(20) {
			random := rand.New(rand.NewPCG(seed, 17))
			f := newSharedFixture(t)
			samples := []sample{{}}
			limiters := make([]*Limiter, 5)
			for i := range limiters {
				limiters[i] = f.limiter(WithRate(rate), WithBurst(burst))
			}
			elapsed, permitted := time.Duration(0), 0
			for range 300 {
				limiter := limiters[random.IntN(len(limiters))]
				switch random.IntN(3) {
				case 0:
					step := time.Duration(random.IntN(150)) * time.Millisecond
					f.clock.Advance(step)
					elapsed += step
				case 1:
					permitted += call(limiter, random.IntN(40))
				default:
					synchronize(limiter)
				}
				held := int64(0)
				for _, l := range limiters {
					held += l.Stats().Allowance
				}
				samples = append(samples, sample{elapsed, permitted, held})
			}
			if permitted < burst {
				t.Fatalf("seed %d: only %d calls were permitted", seed, permitted)
			}
			for i, from := range samples {
				for _, to := range samples[i:] {
					bound := burst + rate*(to.at-from.at).Seconds() + float64(from.held)
					if got := to.permitted - from.permitted; float64(got) > bound+1e-6 {
						t.Fatalf("seed %d: %d calls from %v to %v, want at most %v", seed, got, from.at, to.at, bound)
					}
				}
			}
		}
	})

	t.Run("RL-26 limiters under heavy load use the full shared rate", func(t *testing.T) {
		f := newSharedFixture(t)
		limiters := make([]*Limiter, 10)
		for i := range limiters {
			limiters[i] = f.limiter(WithRate(rate), WithBurst(burst))
		}
		permitted := make([]int, len(limiters))
		total := 0
		const rounds = 200
		for round := range rounds {
			for i := range limiters {
				index := (round + i) % len(limiters)
				got := call(limiters[index], 200)
				permitted[index] += got
				total += got
				synchronize(limiters[index])
			}
			f.clock.Advance(interval)
		}
		low, high := int(rate*(rounds-1)*interval.Seconds()), int(burst+rate*rounds*interval.Seconds())
		if total < low || total > high {
			t.Fatalf("permitted calls = %d, want from %d to %d", total, low, high)
		}
		for i, count := range permitted {
			if count == 0 {
				t.Errorf("limiter %d permitted no call", i)
			}
		}
	})
}

type storeFunc func(ctx context.Context) (Grant, error)

func (f storeFunc) Acquire(ctx context.Context, _ string, _ Acquire) (Grant, error) {
	return f(ctx)
}

func TestLocalMode(t *testing.T) {
	t.Run("RL-27 TK-32 the limiter changes to local mode at the fault threshold", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithLocalRate(10))
		f.store.down.Store(true)
		call(limiter, 4)
		synchronize(limiter)
		call(limiter, 3)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Distributed, Permitted: 7, StoreErrors: 2, Allowance: 3})
		f.clock.Advance(time.Hour)
		assertStats(t, limiter, Stats{Mode: Distributed, Permitted: 7, StoreErrors: 2, Allowance: 3})
		call(limiter, 2)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 9, StoreErrors: 3, Allowance: 1})
		f.clock.Advance(300 * time.Millisecond)
		assertPermitted(t, limiter, 4)
		f.clock.Advance(time.Hour)
		assertPermitted(t, limiter, 5)
	})

	t.Run("RL-27 the limiter keeps the tokens that it holds when they are below the local burst", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithFaultThreshold(1))
		call(limiter, 7)
		f.store.down.Store(true)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 7, StoreErrors: 1, Allowance: 3})
		f.clock.Advance(20 * time.Millisecond)
		assertPermitted(t, limiter, 5)
	})

	t.Run("RL-27 the limiter keeps at most the local burst", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithFaultThreshold(1), WithLocalRate(10))
		call(limiter, 4)
		f.store.down.Store(true)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 4, StoreErrors: 1, Allowance: 5})
	})

	t.Run("RL-28 the tokens that the limiter gives back in a failed synchronization do not return", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		f.store.down.Store(true)
		synchronize(limiter)
		assertStats(t, limiter, Stats{Mode: Distributed, StoreErrors: 1, Allowance: 1})
	})

	t.Run("RL-27 RL-34 the local burst is the share of the burst that the local rate gives", func(t *testing.T) {
		for localRate, want := range map[float64]int{100: 50, 10: 5, 1: 1, 3: 2} {
			f := newSharedFixture(t)
			f.store.down.Store(true)
			limiter := f.limiter(WithLocalRate(localRate))
			if got := permitted(limiter); got != want {
				t.Errorf("local rate %v: permitted calls = %d, want %d", localRate, got, want)
			}
		}
	})

	t.Run("RL-27 a successful synchronization starts the count of faults again", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter()
		for range 3 {
			f.store.down.Store(true)
			synchronize(limiter)
			synchronize(limiter)
			if mode := limiter.Stats().Mode; mode != Distributed {
				t.Fatalf("mode after 2 faults in sequence = %v, want distributed", mode)
			}
			f.store.down.Store(false)
			synchronize(limiter)
		}
		assertStats(t, limiter, Stats{Mode: Distributed, StoreErrors: 6, Allowance: 1})
	})

	t.Run("RL-28 TK-33 a store that does not answer blocks no call and ends at the store timeout", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		store := storeFunc(func(ctx context.Context) (Grant, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return Grant{}, ctx.Err()
		})
		clock := faultvanetest.NewClock(testStart)
		limiter, err := New(WithClock(clock), WithStore(store), WithName(sharedName), manualSync,
			WithStoreTimeout(time.Millisecond), WithSyncInterval(2*time.Millisecond), WithBurst(5))
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		done := make(chan struct{})
		go func() {
			defer close(done)
			synchronize(limiter)
		}()
		<-entered
		if err := limiter.Do(context.Background(), func(context.Context) error { return errDependency }); err != errDependency {
			t.Fatalf("Do() during a store operation = %v, want the error of the call", err)
		}
		<-done
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 1, StoreErrors: 2, Allowance: 4})
	})

	t.Run("RL-28 TK-33 a store fault fails no call", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithFaultThreshold(1))
		f.store.down.Store(true)
		for range 5 {
			synchronize(limiter)
			f.clock.Advance(time.Second)
			if !limiter.Allow() {
				t.Fatal("a call was rejected with tokens in the local bucket")
			}
		}
	})

	t.Run("RL-29 TK-34 the first success returns the limiter to distributed mode", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithFaultThreshold(1))
		f.store.down.Store(true)
		synchronize(limiter)
		before := f.store.calls()
		for range 4 {
			f.clock.Advance(time.Second)
			synchronize(limiter)
		}
		if got := f.store.calls() - before; got != 4 {
			t.Fatalf("store calls in local mode = %d, want one for each synchronization", got)
		}
		assertStats(t, limiter, Stats{Mode: Local, StoreErrors: 5, Allowance: 50})
		call(limiter, 6)
		f.store.down.Store(false)
		synchronize(limiter)
		if request := f.store.last(); request.Wanted != 12 || request.Returned != 0 {
			t.Fatalf("request from local mode = %+v, want 12 tokens and none back", request)
		}
		assertStats(t, limiter, Stats{Mode: Distributed, Permitted: 6, StoreErrors: 5, Allowance: 12})
		f.clock.Advance(time.Hour)
		assertPermitted(t, limiter, 12)
	})

	t.Run("RL-30 TK-30 a limiter starts in local mode with a full bucket when the store is not available", func(t *testing.T) {
		f := newSharedFixture(t)
		f.store.down.Store(true)
		limiter := f.limiter(WithLocalRate(20))
		assertStats(t, limiter, Stats{Mode: Local, StoreErrors: 1, Allowance: 10})
		assertPermitted(t, limiter, 10)
		f.clock.Advance(100 * time.Millisecond)
		assertPermitted(t, limiter, 2)
	})
}

func TestLifecycle(t *testing.T) {
	background := func(t *testing.T, store Store) *Limiter {
		t.Helper()
		limiter, err := New(WithStore(store), WithName(sharedName), WithRate(1000), WithBurst(100),
			WithSyncInterval(2*time.Millisecond), WithStoreTimeout(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		return limiter
	}

	t.Run("RL-23 RL-31 TK-15 the synchronizer runs until Close stops it", func(t *testing.T) {
		before := runtime.NumGoroutine()
		store := &recordingStore{Store: NewMemoryStore(faultvane.SystemClock())}
		limiter := background(t, store)
		eventually(t, "the synchronizer calls the store", func() bool { return store.calls() >= 5 })
		for range 2 {
			if err := limiter.Close(context.Background()); err != nil {
				t.Fatalf("Close() = %v", err)
			}
		}
		calls := store.calls()
		eventually(t, "the goroutine of the synchronizer ends", func() bool { return runtime.NumGoroutine() <= before })
		if store.calls() != calls {
			t.Fatal("the limiter called the store after Close")
		}
	})

	t.Run("RL-24 the synchronizer keeps one token and gets a larger batch after calls", func(t *testing.T) {
		limiter := background(t, NewMemoryStore(faultvane.SystemClock()))
		defer func() { _ = limiter.Close(context.Background()) }()
		eventually(t, "an idle limiter keeps one token", func() bool {
			return limiter.Stats() == Stats{Mode: Distributed, Allowance: 1}
		})
		eventually(t, "the allowance follows the demand", func() bool {
			call(limiter, 20)
			stats := limiter.Stats()
			return stats.Mode == Distributed && stats.Allowance > 2
		})
	})

	t.Run("RL-31 a closed limiter is in local mode and keeps its tokens", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithLocalRate(10))
		if err := limiter.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertStats(t, limiter, Stats{Mode: Local, Allowance: 5})
		assertPermitted(t, limiter, 5)
		f.clock.Advance(200 * time.Millisecond)
		assertPermitted(t, limiter, 2)
	})

	t.Run("RL-31 TK-13 Close returns the context error when the synchronizer does not stop in time", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int64
		limiter := background(t, storeFunc(func(context.Context) (Grant, error) {
			if calls.Add(1) == 2 {
				close(entered)
				<-release
			}
			return Grant{}, errStoreDown
		}))
		<-entered
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := limiter.Close(ctx); err != context.Canceled {
			t.Fatalf("Close() with an ended context = %v, want context.Canceled", err)
		}
		close(release)
		if err := limiter.Close(context.Background()); err != nil {
			t.Fatalf("Close() = %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("store calls = %d, want 2", got)
		}
	})

	t.Run("RL-31 Close of a limiter without a store does nothing", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		if err := limiter.Close(context.Background()); err != nil {
			t.Fatalf("Close() = %v", err)
		}
		assertStats(t, limiter, Stats{Mode: Local, Allowance: 5})
	})
}

func TestStats(t *testing.T) {
	t.Run("RL-35 TK-40 a limiter without a store counts its decisions", func(t *testing.T) {
		limiter, clock := newTestLimiter(t)
		assertStats(t, limiter, Stats{Mode: Local, Allowance: 5})
		call(limiter, 7)
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 5, Rejected: 2})
		clock.Advance(250 * time.Millisecond)
		assertStats(t, limiter, Stats{Mode: Local, Permitted: 5, Rejected: 2, Allowance: 2})
	})

	t.Run("RL-35 a call that the context stops is not counted", func(t *testing.T) {
		limiter, _ := newTestLimiter(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = limiter.Do(ctx, func(context.Context) error { return nil })
		assertStats(t, limiter, Stats{Mode: Local, Allowance: 5})
	})

	t.Run("RL-35 the allowance of a very large bucket has a maximum", func(t *testing.T) {
		limiter, err := New(WithBurst(1 << 62 * 1.5))
		if err != nil {
			t.Fatal(err)
		}
		assertStats(t, limiter, Stats{Mode: Local, Allowance: maxWhole})
	})

	t.Run("RL-35 the names of the modes are fixed", func(t *testing.T) {
		if Distributed.String() != "distributed" || Local.String() != "local" {
			t.Fatalf("names = %q, %q", Distributed, Local)
		}
	})
}

func TestSharedConcurrency(t *testing.T) {
	t.Run("RL-26 TK-14 concurrent calls and synchronizations permit at most the burst and the refill", func(t *testing.T) {
		f := newSharedFixture(t)
		var allowed atomic.Int64
		var group sync.WaitGroup
		start := func(work func()) {
			group.Add(1)
			go func() {
				defer group.Done()
				work()
			}()
		}
		for range 4 {
			limiter := f.limiter(WithRate(1000), WithBurst(100), WithStoreTimeout(time.Hour), WithSyncInterval(2*time.Hour))
			for range 4 {
				start(func() {
					for range 2000 {
						if limiter.Allow() {
							allowed.Add(1)
						}
						_ = limiter.Stats()
					}
				})
			}
			start(func() {
				for range 200 {
					synchronize(limiter)
				}
			})
		}
		start(func() {
			for range 50 {
				f.clock.Advance(time.Millisecond)
			}
		})
		group.Wait()
		if got := allowed.Load(); got < 100 || got > 150 {
			t.Fatalf("permitted calls = %d, want from 100 to 150", got)
		}
	})
}

func TestSharedResources(t *testing.T) {
	t.Run("RL-41 RL-19 TK-50 a call on a limiter with a store makes no allocation", func(t *testing.T) {
		f := newSharedFixture(t)
		limiter := f.limiter(WithRate(1e6), WithBurst(1e6))
		ctx := context.Background()
		run := func(context.Context) error { return nil }
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Do(ctx, run) }); allocations != 0 {
			t.Errorf("Do: %v allocations for each call, want 0", allocations)
		}
		if !limiter.Allow() {
			t.Fatal("the allowance became empty during the measurement")
		}
		assertPermitted(t, limiter, int(limiter.Stats().Allowance))
		if allocations := testing.AllocsPerRun(1000, func() { _ = limiter.Allow() }); allocations != 0 {
			t.Errorf("rejected Allow: %v allocations for each call, want 0", allocations)
		}
	})
}
