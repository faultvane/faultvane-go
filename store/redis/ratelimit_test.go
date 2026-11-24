package redisstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/ratelimit/storetest"
	"github.com/redis/go-redis/v9"
)

const rateLimitName = "payment-api"

func rateLimitStore(t *testing.T, client redis.Scripter, opts ...Option) *RateLimitStore {
	t.Helper()
	namespace := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))
	store, err := NewRateLimitStore(client, append([]Option{WithNamespace(namespace)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func rateLimitAcquire(t *testing.T, store *RateLimitStore, request ratelimit.Acquire) ratelimit.Grant {
	t.Helper()
	grant, err := store.Acquire(context.Background(), rateLimitName, request)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func rateLimitLimiter(t *testing.T, store ratelimit.Store, opts ...ratelimit.Option) *ratelimit.Limiter {
	t.Helper()
	limiter, err := ratelimit.New(append([]ratelimit.Option{ratelimit.WithStore(store), ratelimit.WithName(rateLimitName)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close(context.Background()) })
	return limiter
}

func TestRateLimitConformance(t *testing.T) {
	t.Run("RL-21 RL-37 QA-8 standalone Redis", func(t *testing.T) {
		client := reachable(t, standalone())
		storetest.Run(t, func(t *testing.T) ratelimit.Store { return rateLimitStore(t, client) })
	})
}

func TestRateLimitStore(t *testing.T) {
	client := reachable(t, standalone())
	ctx := context.Background()

	t.Run("RL-37 TK-21 an invalid client, namespace, or key prefix is rejected", func(t *testing.T) {
		invalid := map[string]func() (*RateLimitStore, error){
			"nil client":   func() (*RateLimitStore, error) { return NewRateLimitStore(nil, WithNamespace("a")) },
			"no namespace": func() (*RateLimitStore, error) { return NewRateLimitStore(client) },
			"empty key prefix": func() (*RateLimitStore, error) {
				return NewRateLimitStore(client, WithNamespace("a"), WithKeyPrefix(""))
			},
			"namespace a colon": func() (*RateLimitStore, error) { return NewRateLimitStore(client, WithNamespace("a:b")) },
		}
		for name, create := range invalid {
			if store, err := create(); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Errorf("%s: NewRateLimitStore() = %v, %v, want nil and ErrInvalidConfig", name, store, err)
			}
		}
	})

	t.Run("RL-37 the key name is deterministic and has a hash tag", func(t *testing.T) {
		store, err := NewRateLimitStore(client, WithNamespace("production"))
		if err != nil || store.key(rateLimitName) != "faultvane:{production:payment-api}:rl:bucket" {
			t.Fatalf("key = %q, %v", store.key(rateLimitName), err)
		}
		custom, err := NewRateLimitStore(client, WithNamespace("azAZ09._-"), WithKeyPrefix("app"))
		if err != nil || custom.key("x") != "app:{azAZ09._-:x}:rl:bucket" {
			t.Fatalf("key with a prefix = %q, %v", custom.key("x"), err)
		}
	})

	t.Run("RL-37 the bucket is one hash with the tokens, the store time, and a time to live", func(t *testing.T) {
		store := rateLimitStore(t, client)
		grant := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		key := store.key(rateLimitName)
		want := map[string]string{"v": "1", "tk": "7", "ts": fmt.Sprint(grant.StoreTime.UnixMicro())}
		if got := client.HGetAll(ctx, key).Val(); fmt.Sprint(got) != fmt.Sprint(want) || grant.Tokens != 3 {
			t.Fatalf("hash = %v after a grant of %d, want %v after 3", got, grant.Tokens, want)
		}
		if keys := client.Keys(ctx, "*"+store.store.namespace+"*").Val(); len(keys) != 1 || keys[0] != key {
			t.Fatalf("keys = %v, want only %s", keys, key)
		}
		if ttl := client.PTTL(ctx, key).Val(); ttl <= 59*time.Minute || ttl > time.Hour {
			t.Fatalf("time to live = %v, want about 1h", ttl)
		}
		rateLimitAcquire(t, store, ratelimit.Acquire{Rate: 1e-9, Burst: 10, StateTTL: 3 * time.Hour})
		if ttl := client.PTTL(ctx, key).Val(); ttl <= 2*time.Hour {
			t.Fatalf("time to live after the second operation = %v, want about 3h", ttl)
		}
	})

	t.Run("RL-37 a part of a token stays in the bucket", func(t *testing.T) {
		store := rateLimitStore(t, client)
		first := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		var second ratelimit.Grant
		eventually(t, "the time of Redis moves", func() bool {
			second = rateLimitAcquire(t, store, ratelimit.Acquire{Rate: 0.5, Burst: 10, StateTTL: time.Hour})
			return second.StoreTime.After(first.StoreTime)
		})
		want := 0.5 * second.StoreTime.Sub(first.StoreTime).Seconds()
		got, err := client.HGet(ctx, store.key(rateLimitName), "tk").Float64()
		if err != nil || second.Tokens != 0 || got <= 0 || got > want*1.000001 {
			t.Fatalf("tokens in Redis = %v, %v with a grant of %d, want above 0 and at most %v with no grant", got, err, second.Tokens, want)
		}
	})

	t.Run("RL-37 a time of Redis that goes back gives no tokens and no earlier store time", func(t *testing.T) {
		store := rateLimitStore(t, client)
		drained := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 10, Rate: 1000, Burst: 10, StateTTL: time.Hour})
		later := drained.StoreTime.Add(time.Hour)
		if err := client.HSet(ctx, store.key(rateLimitName), "ts", later.UnixMicro()).Err(); err != nil {
			t.Fatal(err)
		}
		grant := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 10, Rate: 1000, Burst: 10, StateTTL: time.Hour})
		if grant.Tokens != 0 || !grant.StoreTime.Equal(later) {
			t.Fatalf("grant = %+v, want no tokens at %v", grant, later)
		}
		if got := client.HGetAll(ctx, store.key(rateLimitName)).Val(); got["tk"] != "0" || got["ts"] != fmt.Sprint(later.UnixMicro()) {
			t.Fatalf("hash = %v, want no tokens at %d", got, later.UnixMicro())
		}
	})

	t.Run("RL-37 token numbers above 1e14 keep all digits", func(t *testing.T) {
		store := rateLimitStore(t, client)
		grant := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 3_000_000_000_000_001, Rate: 1e-9, Burst: 4_000_000_000_000_003, StateTTL: time.Hour})
		if grant.Tokens != 3_000_000_000_000_001 {
			t.Fatalf("grant = %d, want 3000000000000001", grant.Tokens)
		}
		if got := client.HGet(ctx, store.key(rateLimitName), "tk").Val(); got != "1000000000000002" {
			t.Fatalf("tokens in Redis = %s, want 1000000000000002", got)
		}
	})

	t.Run("RL-38 the store sends the script again after Redis lost its script cache", func(t *testing.T) {
		store := rateLimitStore(t, client)
		rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 1, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		if err := client.ScriptFlush(ctx).Err(); err != nil {
			t.Fatal(err)
		}
		if grant := rateLimitAcquire(t, store, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}); grant.Tokens != 9 {
			t.Fatalf("grant after SCRIPT FLUSH = %+v, want 9 tokens", grant)
		}
	})

	t.Run("RL-38 an operation on a Redis that does not answer returns ErrStoreUnavailable", func(t *testing.T) {
		closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		defer func() { _ = closed.Close() }()
		grant, err := rateLimitStore(t, closed).Acquire(ctx, rateLimitName, ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1})
		if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "redisstore: rate limit acquire: faultvane: store unavailable: ") || grant != (ratelimit.Grant{}) {
			t.Fatalf("Acquire() = %+v, %v", grant, err)
		}
	})

	t.Run("RL-38 an operation stops when its context ends", func(t *testing.T) {
		ended, cancel := context.WithCancel(ctx)
		cancel()
		_, err := rateLimitStore(t, client).Acquire(ended, rateLimitName, ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
	})

	t.Run("RL-38 a value in Redis that is not a bucket returns ErrStoreUnavailable", func(t *testing.T) {
		store := rateLimitStore(t, client)
		if err := client.Set(ctx, store.key(rateLimitName), "text", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(ctx, rateLimitName, ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1}); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrStoreUnavailable", err)
		}
	})
}

type rateLimitReply struct{ value any }

func (r rateLimitReply) reply(ctx context.Context) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	cmd.SetVal(r.value)
	return cmd
}

func (r rateLimitReply) Eval(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return r.reply(ctx)
}

func (r rateLimitReply) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return r.reply(ctx)
}

func (r rateLimitReply) EvalRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return r.reply(ctx)
}

func (r rateLimitReply) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return r.reply(ctx)
}

func (r rateLimitReply) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx)
}

func (r rateLimitReply) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	return redis.NewStringCmd(ctx)
}

func TestRateLimitReply(t *testing.T) {
	acquire := func(t *testing.T, reply any) (ratelimit.Grant, error) {
		t.Helper()
		return rateLimitStore(t, rateLimitReply{reply}).Acquire(context.Background(), rateLimitName, ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1})
	}

	t.Run("RL-38 a reply of integers and strings gives a grant", func(t *testing.T) {
		grant, err := acquire(t, []any{int64(7), "1700000000000002"})
		if want := (ratelimit.Grant{Tokens: 7, StoreTime: time.UnixMicro(1700000000000002)}); err != nil || grant != want {
			t.Fatalf("Acquire() = %+v, %v, want %+v", grant, err, want)
		}
	})

	for name, reply := range map[string]any{
		"RL-38 a reply that is not a list":                "OK",
		"RL-38 a reply with 1 value":                      []any{"1"},
		"RL-38 a reply with 3 values":                     []any{"1", "2", "3"},
		"RL-38 a reply with tokens that are no number":    []any{"x", "2"},
		"RL-38 a reply with a time of an unexpected type": []any{"1", 2.5},
	} {
		t.Run(name+" returns ErrStoreUnavailable", func(t *testing.T) {
			grant, err := acquire(t, reply)
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "redisstore: rate limit acquire: ") || grant != (ratelimit.Grant{}) {
				t.Fatalf("Acquire() = %+v, %v", grant, err)
			}
		})
	}
}

func rateLimitStoreTime(t *testing.T, store *RateLimitStore) time.Time {
	t.Helper()
	grant, err := store.Acquire(context.Background(), "clock", ratelimit.Acquire{Rate: 1, Burst: 1, StateTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return grant.StoreTime
}

func TestRateLimitAcceptance(t *testing.T) {
	t.Run("RL-39 RL-26 RL-23 ten limiters under heavy load share one rate through Redis", func(t *testing.T) {
		const (
			rate     = 500.0
			burst    = 50
			duration = 2 * time.Second
		)
		store := rateLimitStore(t, reachable(t, standalone()))
		counting := &rateLimitCounter{Store: store}
		started := rateLimitStoreTime(t, store)
		var permitted atomic.Int64
		var group sync.WaitGroup
		stop := make(chan struct{})
		limiters := make([]*ratelimit.Limiter, 10)
		for i := range limiters {
			limiter := rateLimitLimiter(t, counting, ratelimit.WithRate(rate), ratelimit.WithBurst(burst), ratelimit.WithLocalRate(1e-6), ratelimit.WithFaultThreshold(1<<30))
			limiters[i] = limiter
			group.Add(1)
			go func() {
				defer group.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if limiter.Allow() {
						permitted.Add(1)
					}
					time.Sleep(50 * time.Microsecond)
				}
			}()
		}
		time.Sleep(duration)
		close(stop)
		group.Wait()
		elapsed := rateLimitStoreTime(t, store).Sub(started).Seconds()
		calls := int64(0)
		for _, limiter := range limiters {
			stats := limiter.Stats()
			calls += stats.Permitted + stats.Rejected
		}
		got, most, least := float64(permitted.Load()), burst+rate*elapsed+float64(len(limiters)), 0.7*rate*elapsed
		if got > most || got < least {
			t.Fatalf("permitted calls = %v of %d in %.3f s, want from %.0f to %.0f", got, calls, elapsed, least, most)
		}
		if operations, limit := counting.calls.Load(), int64(len(limiters))*(2+int64(elapsed/0.1)); operations > limit || calls < 10*operations {
			t.Fatalf("store operations = %d for %d calls, want at most %d", operations, calls, limit)
		}
	})

	t.Run("RL-40 RL-27 RL-29 TK-32 the limiters use the local rate during a Redis outage and share the rate again after it", func(t *testing.T) {
		const (
			localRate  = 100.0
			localBurst = 10
		)
		namespace := rateLimitStore(t, reachable(t, standalone())).store.namespace
		proxies := make([]*proxy, 3)
		limiters := make([]*ratelimit.Limiter, len(proxies))
		for i := range limiters {
			proxies[i] = newProxy(t, standalone())
			client := redis.NewClient(&redis.Options{Addr: proxies[i].listener.Addr().String(), DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, MaxRetries: -1})
			t.Cleanup(func() { _ = client.Close() })
			store, err := NewRateLimitStore(client, WithNamespace(namespace))
			if err != nil {
				t.Fatal(err)
			}
			limiters[i] = rateLimitLimiter(t, store, ratelimit.WithRate(1000), ratelimit.WithBurst(100), ratelimit.WithLocalRate(localRate),
				ratelimit.WithSyncInterval(40*time.Millisecond), ratelimit.WithStoreTimeout(30*time.Millisecond), ratelimit.WithFaultThreshold(2))
		}
		all := func(mode ratelimit.Mode) func() bool {
			return func() bool {
				for _, limiter := range limiters {
					_ = limiter.Allow()
					if limiter.Stats().Mode != mode {
						return false
					}
				}
				return true
			}
		}
		eventually(t, "all limiters are in distributed mode", all(ratelimit.Distributed))
		for _, p := range proxies {
			p.stop()
		}
		eventually(t, "all limiters are in local mode", all(ratelimit.Local))
		for i, limiter := range limiters {
			started, count := time.Now(), 0
			for count < 3*localBurst {
				if limiter.Allow() {
					count++
				}
				if time.Since(started) > 15*time.Second {
					t.Fatalf("limiter %d permitted %d calls in local mode, want %d", i, count, 3*localBurst)
				}
				time.Sleep(100 * time.Microsecond)
			}
			if most := localBurst + localRate*time.Since(started).Seconds(); float64(count) > most {
				t.Fatalf("limiter %d permitted %d calls in local mode, want at most %.1f", i, count, most)
			}
			if stats := limiter.Stats(); stats.Mode != ratelimit.Local || stats.StoreErrors < 2 {
				t.Fatalf("limiter %d: stats = %+v, want local mode after 2 store errors", i, stats)
			}
		}
		for _, p := range proxies {
			p.start()
		}
		eventually(t, "all limiters are in distributed mode again", all(ratelimit.Distributed))
		eventually(t, "a limiter permits a call from the shared bucket", func() bool {
			return limiters[0].Allow() && limiters[0].Stats().Mode == ratelimit.Distributed
		})
	})
}

type rateLimitCounter struct {
	ratelimit.Store
	calls atomic.Int64
}

func (s *rateLimitCounter) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	s.calls.Add(1)
	return s.Store.Acquire(ctx, name, request)
}
