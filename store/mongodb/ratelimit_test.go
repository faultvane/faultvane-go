package mongostore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/ratelimit/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const rateLimitName = "payment-api"

type storedRateLimit struct {
	Version  int64     `bson:"version"`
	Tokens   float64   `bson:"tokens"`
	At       time.Time `bson:"at"`
	ExpireAt time.Time `bson:"expireAt"`
}

func rateLimitStore(t *testing.T, c *mongo.Collection) *RateLimitStore {
	t.Helper()
	store, err := NewRateLimitStore(c, WithNamespace(fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func rateLimitAcquire(t *testing.T, store ratelimit.Store, name string, request ratelimit.Acquire) ratelimit.Grant {
	t.Helper()
	grant, err := store.Acquire(context.Background(), name, request)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func rateLimitStored(t *testing.T, c *mongo.Collection, store *RateLimitStore, name string) storedRateLimit {
	t.Helper()
	var doc storedRateLimit
	if err := c.FindOne(context.Background(), bson.D{{Key: "_id", Value: store.id(name)}}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func rateLimitSet(t *testing.T, c *mongo.Collection, store *RateLimitStore, field string, value any) {
	t.Helper()
	if _, err := c.UpdateOne(context.Background(), bson.D{{Key: "_id", Value: store.id(rateLimitName)}}, bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: value}}}}); err != nil {
		t.Fatal(err)
	}
}

func rateLimitAfterRead(t *testing.T, store *RateLimitStore, react func()) *RateLimitStore {
	t.Helper()
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, succeeded *event.CommandSucceededEvent) {
		if succeeded.CommandName == "findAndModify" {
			react()
		}
	}}
	client, err := mongo.Connect(options.Client().ApplyURI(uri()).SetMonitor(monitor))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	observed, err := NewRateLimitStore(client.Database("faultvane_test").Collection("circuit_breakers"), WithNamespace(store.store.namespace))
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func rateLimitLimiter(t *testing.T, store ratelimit.Store, opts ...ratelimit.Option) *ratelimit.Limiter {
	t.Helper()
	limiter, err := ratelimit.New(append([]ratelimit.Option{ratelimit.WithStore(store), ratelimit.WithName(rateLimitName), ratelimit.WithLogger(quiet)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close(context.Background()) })
	return limiter
}

func TestRateLimitConformance(t *testing.T) {
	t.Run("RL-21 RL-54 QA-8 the rate limit store contract with MongoDB", func(t *testing.T) {
		c := collection(t)
		storetest.Run(t, func(t *testing.T) ratelimit.Store { return rateLimitStore(t, c) })
	})
}

func TestRateLimitNew(t *testing.T) {
	c := connect(t, uri())

	t.Run("RL-54 CFG-3 an invalid collection or namespace is rejected", func(t *testing.T) {
		invalid := map[string]func() (*RateLimitStore, error){
			"nil collection":         func() (*RateLimitStore, error) { return NewRateLimitStore(nil, WithNamespace("a")) },
			"no namespace":           func() (*RateLimitStore, error) { return NewRateLimitStore(c) },
			"namespace with a colon": func() (*RateLimitStore, error) { return NewRateLimitStore(c, WithNamespace("a:b")) },
		}
		for name, create := range invalid {
			if store, err := create(); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Errorf("%s: NewRateLimitStore() = %v, %v, want nil and ErrInvalidConfig", name, store, err)
			}
		}
	})

	t.Run("RL-54 the document identifier is the namespace, the name, and a fixed suffix", func(t *testing.T) {
		store, err := NewRateLimitStore(c, WithNamespace("azAZ09._-"))
		if err != nil || store.id(rateLimitName) != "azAZ09._-:payment-api:rl:bucket" {
			t.Fatalf("id = %q, err = %v", store.id(rateLimitName), err)
		}
	})
}

func TestRateLimitData(t *testing.T) {
	ctx := context.Background()
	c := collection(t)

	t.Run("RL-54 RL-56 one document for each name holds the version, the tokens, the store time, and the expiry time", func(t *testing.T) {
		store := rateLimitStore(t, c)
		grant := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		rateLimitAcquire(t, store, "other", ratelimit.Acquire{Wanted: 1, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		want := storedRateLimit{Version: 1, Tokens: 7, At: grant.StoreTime, ExpireAt: grant.StoreTime.Add(time.Hour)}
		if got := rateLimitStored(t, c, store, rateLimitName); got != want || grant.Tokens != 3 {
			t.Fatalf("document = %+v after a grant of %d, want %+v after 3", got, grant.Tokens, want)
		}
		if skew := time.Since(grant.StoreTime).Abs(); skew > time.Hour {
			t.Fatalf("store time %v is %v from the local time", grant.StoreTime, skew)
		}
		if count, err := c.CountDocuments(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$regex", Value: "^" + store.store.namespace + ":"}}}}); err != nil || count != 2 {
			t.Fatalf("documents of the namespace = %d, %v, want 2", count, err)
		}
		later := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Rate: 1e-9, Burst: 10, StateTTL: 3 * time.Hour})
		got := rateLimitStored(t, c, store, rateLimitName)
		if got.Version != 2 || !got.At.Equal(later.StoreTime) || !got.ExpireAt.Equal(later.StoreTime.Add(3*time.Hour)) || math.Abs(got.Tokens-7) > 1e-6 {
			t.Fatalf("document after the second operation = %+v, want version 2 with 7 tokens that expires 3h after %v", got, later.StoreTime)
		}
	})

	t.Run("RL-54 a circuit breaker and a rate limit with one namespace and one name share a collection", func(t *testing.T) {
		store := rateLimitStore(t, c)
		breakers, err := New(c, WithNamespace(store.store.namespace))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := breakers.Report(ctx, rateLimitName, report(5, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if grant := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}); grant.Tokens != 10 {
			t.Fatalf("grant = %+v, want 10 tokens", grant)
		}
		if snapshot, err := breakers.Load(ctx, rateLimitName); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("Load() = %+v, %v, want open generation 1", snapshot, err)
		}
	})

	t.Run("RL-54 a part of a token stays in the bucket", func(t *testing.T) {
		store := rateLimitStore(t, c)
		first := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		var second ratelimit.Grant
		eventually(t, "the time of MongoDB moves", func() bool {
			second = rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Rate: 0.5, Burst: 10, StateTTL: time.Hour})
			return second.StoreTime.After(first.StoreTime)
		})
		want := 0.5 * second.StoreTime.Sub(first.StoreTime).Seconds()
		if got := rateLimitStored(t, c, store, rateLimitName).Tokens; second.Tokens != 0 || got <= 0 || got > want*1.000001 {
			t.Fatalf("tokens in MongoDB = %v with a grant of %d, want above 0 and at most %v with no grant", got, second.Tokens, want)
		}
	})

	t.Run("RL-54 the bucket refills by the time of MongoDB, not by the time of the instance", func(t *testing.T) {
		store := rateLimitStore(t, c)
		drained := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1000, Burst: 10, StateTTL: time.Hour})
		later := drained.StoreTime.Add(time.Hour)
		rateLimitSet(t, c, store, "at", later)
		grant := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1000, Burst: 10, StateTTL: time.Hour})
		if grant.Tokens != 0 || !grant.StoreTime.Equal(later) {
			t.Fatalf("grant = %+v, want no tokens at %v", grant, later)
		}
		if got := rateLimitStored(t, c, store, rateLimitName); got.Tokens != 0 || !got.At.Equal(later) {
			t.Fatalf("document = %+v, want no tokens at %v", got, later)
		}
		rateLimitSet(t, c, store, "at", drained.StoreTime.Add(-time.Hour))
		refilled := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 4, Rate: 0.001, Burst: 10, StateTTL: time.Hour})
		if skew := refilled.StoreTime.Sub(drained.StoreTime); refilled.Tokens != 3 || skew < 0 || skew > time.Minute {
			t.Fatalf("grant = %+v, want 3 tokens for 1h of MongoDB time near %v", refilled, drained.StoreTime)
		}
	})

	t.Run("RL-54 concurrent first operations of new names grant exactly the burst", func(t *testing.T) {
		store := rateLimitStore(t, c)
		const names, burst = 4, 100
		var granted [names]atomic.Int64
		var failed atomic.Value
		var group sync.WaitGroup
		for i := range names * 12 {
			group.Add(1)
			go func() {
				defer group.Done()
				grant, err := store.Acquire(ctx, fmt.Sprintf("new-%d", i%names), ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: burst, StateTTL: time.Hour})
				if err != nil {
					failed.Store(err.Error())
				}
				granted[i%names].Add(grant.Tokens)
			}()
		}
		group.Wait()
		for i := range granted {
			if got := granted[i].Load(); got != burst || failed.Load() != nil {
				t.Fatalf("name %d: %d tokens granted with the error %v, want %d and no error", i, got, failed.Load(), burst)
			}
			if doc := rateLimitStored(t, c, store, fmt.Sprintf("new-%d", i)); doc.Tokens > 1e-6 || doc.Version != 12 {
				t.Fatalf("name %d: document = %+v, want an empty bucket with version 12", i, doc)
			}
		}
	})

	t.Run("RL-54 an operation that loses one race applies its change to the new state", func(t *testing.T) {
		store := rateLimitStore(t, c)
		reads := 0
		observed := rateLimitAfterRead(t, store, func() {
			if reads++; reads == 1 {
				rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
			}
		})
		grant := rateLimitAcquire(t, observed, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		if doc := rateLimitStored(t, c, store, rateLimitName); grant.Tokens != 7 || reads != 2 || doc.Version != 2 || doc.Tokens > 1e-6 {
			t.Fatalf("grant = %+v after %d read steps with the document %+v, want 7 tokens after 2 and an empty bucket with version 2", grant, reads, doc)
		}
	})

	t.Run("RL-55 an operation that loses each race stops after a fixed number of attempts", func(t *testing.T) {
		store := rateLimitStore(t, c)
		reads := 0
		observed := rateLimitAfterRead(t, store, func() {
			reads++
			_, _ = c.UpdateOne(ctx, bson.D{{Key: "_id", Value: store.id(rateLimitName)}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "version", Value: 1}}}})
		})
		grant, err := observed.Acquire(ctx, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		if !errors.Is(err, faultvane.ErrStoreUnavailable) || !errors.Is(err, errRateLimitContention) || reads != maxAttempts || grant != (ratelimit.Grant{}) {
			t.Fatalf("Acquire() = %+v, %v after %d read steps, want a contention error after %d", grant, err, reads, maxAttempts)
		}
		if doc := rateLimitStored(t, c, store, rateLimitName); doc.Tokens != 10 {
			t.Fatalf("document = %+v, want 10 tokens", doc)
		}
	})

	t.Run("RL-55 an operation whose context ends before its write changes nothing", func(t *testing.T) {
		store := rateLimitStore(t, c)
		limited, cancel := context.WithCancel(ctx)
		observed := rateLimitAfterRead(t, store, cancel)
		if _, err := observed.Acquire(limited, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}); !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
		if doc := rateLimitStored(t, c, store, rateLimitName); doc.Tokens != 10 || doc.Version != 0 {
			t.Fatalf("document = %+v, want 10 tokens with version 0", doc)
		}
	})

	t.Run("RL-54 a duplicate key error of two concurrent first operations uses one attempt and is no fault", func(t *testing.T) {
		if err := exceptDuplicateKey(mongo.CommandError{Code: 11000}); err != nil {
			t.Fatalf("exceptDuplicateKey() of a duplicate key error = %v, want nil", err)
		}
		if err := exceptDuplicateKey(context.Canceled); err != context.Canceled {
			t.Fatalf("exceptDuplicateKey() of a different error = %v, want the same error", err)
		}
	})

	t.Run("RL-55 a document that is not a bucket returns ErrStoreUnavailable", func(t *testing.T) {
		store := rateLimitStore(t, c)
		rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		rateLimitSet(t, c, store, "tokens", "text")
		if _, err := store.Acquire(ctx, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrStoreUnavailable", err)
		}
	})

	t.Run("RL-56 EnsureIndexes creates the index that removes documents after their expiry time", func(t *testing.T) {
		store := rateLimitStore(t, c)
		for range 2 {
			if err := store.EnsureIndexes(ctx); err != nil {
				t.Fatal(err)
			}
		}
		specifications, err := c.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(specifications, func(specification mongo.IndexSpecification) bool {
			return specification.Name == "faultvane_expire" && specification.ExpireAfterSeconds != nil && *specification.ExpireAfterSeconds == 0 && strings.Contains(specification.KeysDocument.String(), "expireAt")
		}) {
			t.Fatalf("indexes = %+v, want the index faultvane_expire on expireAt with 0 seconds", specifications)
		}
	})

	t.Run("RL-56 a new document has an expiry time before the first write", func(t *testing.T) {
		store := rateLimitStore(t, c)
		limited, cancel := context.WithCancel(ctx)
		observed := rateLimitAfterRead(t, store, cancel)
		_, _ = observed.Acquire(limited, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		if doc := rateLimitStored(t, c, store, rateLimitName); !doc.ExpireAt.Equal(doc.At.Add(time.Hour)) {
			t.Fatalf("document = %+v, want an expiry time 1h after the store time", doc)
		}
	})
}

func TestRateLimitFaults(t *testing.T) {
	ctx := context.Background()
	request := ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1, StateTTL: time.Hour}

	t.Run("RL-55 each operation on a MongoDB that does not answer returns ErrStoreUnavailable", func(t *testing.T) {
		store := rateLimitStore(t, connect(t, "mongodb://127.0.0.1:1"))
		grant, acquireErr := store.Acquire(ctx, rateLimitName, request)
		failures := map[string]error{"rate limit acquire": acquireErr, "ensure indexes": store.EnsureIndexes(ctx)}
		for operation, err := range failures {
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "mongostore: "+operation+": faultvane: store unavailable: ") {
				t.Errorf("%s: error = %v", operation, err)
			}
		}
		if grant != (ratelimit.Grant{}) {
			t.Errorf("grant of a failed operation = %+v", grant)
		}
	})

	t.Run("RL-55 an operation stops when its context ends and changes nothing", func(t *testing.T) {
		c := collection(t)
		store := rateLimitStore(t, c)
		ended, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Acquire(ended, rateLimitName, request); !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
		if count, err := c.CountDocuments(ctx, bson.D{{Key: "_id", Value: store.id(rateLimitName)}}); err != nil || count != 0 {
			t.Fatalf("documents = %d, %v, want 0", count, err)
		}
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

func rateLimitStoreTime(t *testing.T, store ratelimit.Store) time.Time {
	t.Helper()
	return rateLimitAcquire(t, store, "clock", ratelimit.Acquire{Rate: 1, Burst: 1, StateTTL: time.Minute}).StoreTime
}

func TestRateLimitAcceptance(t *testing.T) {
	t.Run("RL-57 RL-26 RL-23 ten limiters under heavy load share one rate through MongoDB", func(t *testing.T) {
		const (
			rate     = 500.0
			burst    = 50
			duration = 2 * time.Second
		)
		store := rateLimitStore(t, collection(t))
		if err := store.EnsureIndexes(context.Background()); err != nil {
			t.Fatal(err)
		}
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
		calls, faults := int64(0), int64(0)
		for _, limiter := range limiters {
			stats := limiter.Stats()
			calls += stats.Permitted + stats.Rejected
			faults += stats.StoreErrors
		}
		got, most, least := float64(permitted.Load()), burst+rate*elapsed+float64(len(limiters)), 0.7*rate*elapsed
		if got > most || got < least {
			t.Fatalf("permitted calls = %v of %d in %.3f s with %d store faults, want from %.0f to %.0f", got, calls, elapsed, faults, least, most)
		}
		if operations, limit := counting.calls.Load(), int64(len(limiters))*(2+int64(elapsed/0.1)); operations > limit || calls < 10*operations {
			t.Fatalf("store operations = %d for %d calls, want at most %d", operations, calls, limit)
		}
	})

	t.Run("RL-58 RL-27 RL-29 the limiters use the local rate during a MongoDB outage and share the rate again after it", func(t *testing.T) {
		const (
			localRate  = 100.0
			localBurst = 10
		)
		direct := rateLimitStore(t, collection(t))
		if err := direct.EnsureIndexes(context.Background()); err != nil {
			t.Fatal(err)
		}
		links := make([]*link, 3)
		limiters := make([]*ratelimit.Limiter, len(links))
		for i := range limiters {
			links[i] = &link{}
			store, err := NewRateLimitStore(connectThrough(t, links[i]), WithNamespace(direct.store.namespace))
			if err != nil {
				t.Fatal(err)
			}
			limiters[i] = rateLimitLimiter(t, store, ratelimit.WithRate(1000), ratelimit.WithBurst(100), ratelimit.WithLocalRate(localRate),
				ratelimit.WithSyncInterval(100*time.Millisecond), ratelimit.WithStoreTimeout(90*time.Millisecond), ratelimit.WithFaultThreshold(2))
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
		for _, l := range links {
			l.stop()
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
		for _, l := range links {
			l.start()
		}
		eventually(t, "all limiters are in distributed mode again", all(ratelimit.Distributed))
		eventually(t, "a limiter permits a call from the shared bucket", func() bool {
			return limiters[0].Allow() && limiters[0].Stats().Mode == ratelimit.Distributed
		})
	})
}

func BenchmarkRateLimitAcquire(b *testing.B) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri()))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	store, err := NewRateLimitStore(client.Database("faultvane_test").Collection("circuit_breakers"), WithNamespace(fmt.Sprintf("bench-%d", time.Now().UnixNano())))
	if err != nil {
		b.Fatal(err)
	}
	var durations []time.Duration
	for b.Loop() {
		started := time.Now()
		if _, err := store.Acquire(context.Background(), "a", ratelimit.Acquire{Wanted: 1, Rate: 1000, Burst: 100, StateTTL: time.Hour}); err != nil {
			b.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	slices.Sort(durations)
	b.ReportMetric(float64(durations[len(durations)/2].Microseconds())/1000, "p50-ms")
	b.ReportMetric(float64(durations[len(durations)*99/100].Microseconds())/1000, "p99-ms")
}
