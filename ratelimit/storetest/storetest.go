package storetest

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/ratelimit"
)

const (
	burst = 100
	slow  = 1e-9
	fast  = 1000
	long  = time.Hour
)

var nameCounter atomic.Uint64

type recorder struct {
	store    ratelimit.Store
	name     string
	problems []string
}

func newRecorder(store ratelimit.Store) *recorder {
	return &recorder{store: store, name: fmt.Sprintf("conformance-%d-%d", time.Now().UnixNano(), nameCounter.Add(1))}
}

func (r *recorder) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *recorder) expectTrue(step string, ok bool) {
	if !ok {
		r.problem("%s", step)
	}
}

func (r *recorder) acquire(wanted, returned int64, rate float64) ratelimit.Grant {
	grant, err := r.store.Acquire(context.Background(), r.name, ratelimit.Acquire{Wanted: wanted, Returned: returned, Rate: rate, Burst: burst, StateTTL: long})
	if err != nil {
		r.problem("Acquire: error %v", err)
	}
	return grant
}

func (r *recorder) expect(step string, grant ratelimit.Grant, want int64) {
	if grant.Tokens != want {
		r.problem("%s: %d tokens granted, want %d", step, grant.Tokens, want)
	}
}

func (r *recorder) drain() ratelimit.Grant {
	grant := r.acquire(burst, 0, slow)
	r.expect("drain the full bucket", grant, burst)
	return grant
}

type check struct {
	name string
	run  func(r *recorder)
}

var checks = []check{
	{"RS2 a bucket without state is full", func(r *recorder) {
		r.expect("more than the burst wanted", r.acquire(burst+50, 0, slow), burst)
	}},
	{"RS3 a grant is never above the wanted tokens", func(r *recorder) {
		r.expect("30 tokens wanted", r.acquire(30, 0, slow), 30)
		r.expect("no tokens wanted", r.acquire(0, 0, slow), 0)
		r.expect("a negative number wanted", r.acquire(-5, 0, slow), 0)
		r.expect("the remaining tokens", r.acquire(burst, 0, slow), burst-30)
	}},
	{"RS3 a grant is never above the tokens of the bucket", func(r *recorder) {
		r.expect("first request", r.acquire(60, 0, slow), 60)
		r.expect("second request", r.acquire(60, 0, slow), burst-60)
		r.expect("request on an empty bucket", r.acquire(1, 0, slow), 0)
	}},
	{"RS4 the bucket refills by the time of the store", func(r *recorder) {
		drained := r.acquire(burst, 0, fast)
		r.expect("drain the full bucket", drained, burst)
		time.Sleep(20 * time.Millisecond)
		refilled := r.acquire(burst, 0, fast)
		want := fast * refilled.StoreTime.Sub(drained.StoreTime).Seconds()
		r.expectTrue(fmt.Sprintf("%d tokens after %v of store time, want %.3f", refilled.Tokens, refilled.StoreTime.Sub(drained.StoreTime), want),
			want >= 1 && math.Abs(float64(refilled.Tokens)-want) <= 1)
	}},
	{"RS4 the bucket never holds more than the burst", func(r *recorder) {
		r.expect("create the state", r.acquire(0, 0, fast*fast), 0)
		time.Sleep(5 * time.Millisecond)
		r.expect("more than the burst wanted after a refill", r.acquire(burst+50, 0, fast*fast), burst)
	}},
	{"RS4 each response contains a store time that does not go back", func(r *recorder) {
		first := r.acquire(1, 0, slow).StoreTime
		second := r.acquire(1, 0, slow).StoreTime
		r.expectTrue("the store time is zero", !first.IsZero() && !second.IsZero())
		r.expectTrue("the store time went back", !second.Before(first))
	}},
	{"RS5 returned tokens are available in the same operation", func(r *recorder) {
		r.drain()
		r.expect("4 tokens returned and 10 wanted", r.acquire(10, 4, slow), 4)
		r.expect("request on an empty bucket", r.acquire(1, 0, slow), 0)
	}},
	{"RS5 returned tokens never raise the bucket above the burst", func(r *recorder) {
		r.expect("tokens returned to a full bucket", r.acquire(burst+50, 50, slow), burst)
		r.expect("more than the burst returned to an empty bucket", r.acquire(burst+50, burst+50, slow), burst)
	}},
	{"RS6 two names share nothing", func(r *recorder) {
		other := newRecorder(r.store)
		r.drain()
		other.expect("the other name", other.acquire(burst, 0, slow), burst)
		r.problems = append(r.problems, other.problems...)
	}},
	{"RS7 an operation with an ended context returns an error and changes nothing", func(r *recorder) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := r.store.Acquire(ctx, r.name, ratelimit.Acquire{Wanted: burst, Rate: slow, Burst: burst, StateTTL: long})
		r.expectTrue("Acquire returned no error", err != nil)
		r.drain()
	}},
	{"RS1 concurrent operations grant exactly the tokens of the bucket", func(r *recorder) {
		var granted atomic.Int64
		var group sync.WaitGroup
		for range 32 {
			group.Add(1)
			go func() {
				defer group.Done()
				grant, err := r.store.Acquire(context.Background(), r.name, ratelimit.Acquire{Wanted: 10, Rate: slow, Burst: burst, StateTTL: long})
				if err == nil {
					granted.Add(grant.Tokens)
				}
			}()
		}
		group.Wait()
		r.expectTrue(fmt.Sprintf("%d tokens granted to 32 concurrent requests, want %d", granted.Load(), burst), granted.Load() == burst)
	}},
}

func (c check) problems(store ratelimit.Store) []string {
	r := newRecorder(store)
	c.run(r)
	return r.problems
}

func Run(t *testing.T, newStore func(t *testing.T) ratelimit.Store) {
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			for _, problem := range c.problems(newStore(t)) {
				t.Error(problem)
			}
		})
	}
}
