package mongostore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/circuitbreaker/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	namespaceCounter atomic.Uint64
	errDependency    = errors.New("dependency failed")
	quiet            = slog.New(slog.DiscardHandler)
)

func uri() string {
	if value := os.Getenv("FAULTVANE_MONGODB_URI"); value != "" {
		return value
	}
	return "mongodb://127.0.0.1:27099"
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func connect(t *testing.T, target string) *mongo.Collection {
	t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI(target).SetServerSelectionTimeout(500 * time.Millisecond).SetConnectTimeout(500 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client.Database("faultvane_test").Collection("circuit_breakers")
}

func collection(t *testing.T) *mongo.Collection {
	t.Helper()
	c := connect(t, uri())
	eventually(t, "MongoDB answers", func() bool { return c.Database().Client().Ping(context.Background(), nil) == nil })
	return c
}

func newStore(t *testing.T, c *mongo.Collection) *Store {
	t.Helper()
	store, err := New(c, WithNamespace(fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func report(failures int64, ttl time.Duration) circuitbreaker.Report {
	return circuitbreaker.Report{
		Calls:         failures,
		Failures:      failures,
		Thresholds:    circuitbreaker.Thresholds{FailureThreshold: 5, MinimumCalls: 1000, FailureRate: 1},
		WindowSize:    time.Minute,
		WindowBuckets: 6,
		OpenDuration:  time.Hour,
		StateTTL:      ttl,
	}
}

func TestConformance(t *testing.T) {
	t.Run("QA-8 DS-6 GEN-2 the store contract with MongoDB", func(t *testing.T) {
		store := newStore(t, collection(t))
		storetest.Run(t, func(*testing.T) circuitbreaker.Store { return store })
	})
}

func TestNew(t *testing.T) {
	c := connect(t, uri())
	invalid := map[string]func() (*Store, error){
		"CFG-3 nil collection":             func() (*Store, error) { return New(nil, WithNamespace("a")) },
		"CFG-3 no namespace":               func() (*Store, error) { return New(c) },
		"CFG-3 namespace with a colon":     func() (*Store, error) { return New(c, WithNamespace("a:b")) },
		"CFG-3 namespace with a dollar":    func() (*Store, error) { return New(c, WithNamespace("$a")) },
		"CFG-3 namespace with 65 letters":  func() (*Store, error) { return New(c, WithNamespace(strings.Repeat("a", 65))) },
		"CFG-3 namespace outside of ASCII": func() (*Store, error) { return New(c, WithNamespace("café")) },
	}
	for name, create := range invalid {
		t.Run(name, func(t *testing.T) {
			if store, err := create(); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", store, err)
			}
		})
	}

	t.Run("KEY-1 KEY-2 the document identifier is the namespace and the name", func(t *testing.T) {
		store, err := New(c, WithNamespace("azAZ09._-"))
		if err != nil || store.id("payment-service") != "azAZ09._-:payment-service" {
			t.Fatalf("id = %q, err = %v", store.id("payment-service"), err)
		}
		long, err := New(c, WithNamespace(strings.Repeat("a", 64)))
		if err != nil || long == nil {
			t.Fatalf("New() with 64 letters = %v, %v", long, err)
		}
	})
}

func TestData(t *testing.T) {
	ctx := context.Background()
	c := collection(t)

	t.Run("KEY-2 DS-2 one document for each breaker holds the record, the version, and the expiry time", func(t *testing.T) {
		store := newStore(t, c)
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.Generation != 0 || time.Since(snapshot.StoreTime).Abs() > time.Hour {
			t.Fatalf("Load() of a breaker without a document = %+v, %v", snapshot, err)
		}
		if count, _ := c.CountDocuments(ctx, bson.D{{Key: "_id", Value: store.id("a")}}); count != 0 {
			t.Fatalf("Load() created %d documents", count)
		}
		first, err := store.Report(ctx, "a", report(2, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "a", report(1, 2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Version  int64     `bson:"version"`
			Record   string    `bson:"record"`
			ExpireAt time.Time `bson:"expireAt"`
		}
		if err := c.FindOne(ctx, bson.D{{Key: "_id", Value: store.id("a")}}).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		if doc.Version != 2 || !strings.Contains(doc.Record, `"Failures":3`) {
			t.Fatalf("document = %+v, want version 2 with 3 failures", doc)
		}
		if ttl := doc.ExpireAt.Sub(first.StoreTime); ttl < 2*time.Hour || ttl > 2*time.Hour+time.Minute {
			t.Fatalf("the document expires %v after the first report, want about 2h", ttl)
		}
		loaded, err := store.Load(ctx, "a")
		if err != nil || loaded.State != circuitbreaker.Closed || loaded.StoreTime.Before(first.StoreTime) {
			t.Fatalf("Load() = %+v, %v", loaded, err)
		}
	})

	t.Run("TTL-1 TTL-2 EnsureIndexes creates the index that removes documents after their expiry time", func(t *testing.T) {
		store := newStore(t, c)
		if err := store.EnsureIndexes(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureIndexes(ctx); err != nil {
			t.Fatalf("second EnsureIndexes() = %v", err)
		}
		specifications, err := c.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, specification := range specifications {
			if specification.Name == "faultvane_expire" {
				found = specification.ExpireAfterSeconds != nil && *specification.ExpireAfterSeconds == 0 && strings.Contains(specification.KeysDocument.String(), "expireAt")
			}
		}
		if !found {
			t.Fatalf("indexes = %+v, want the index faultvane_expire on expireAt with 0 seconds", specifications)
		}
	})

	t.Run("RF-1 a record that is not valid is a store fault", func(t *testing.T) {
		store := newStore(t, c)
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.UpdateOne(ctx, bson.D{{Key: "_id", Value: store.id("a")}}, bson.D{{Key: "$set", Value: bson.D{{Key: "record", Value: "not json"}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(ctx, "a"); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Load() error = %v, want ErrStoreUnavailable", err)
		}
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Report() error = %v, want ErrStoreUnavailable", err)
		}
	})

	t.Run("GEN-2 RF-3 an operation that loses each race stops after a fixed number of attempts", func(t *testing.T) {
		store := newStore(t, c)
		attempts := 0
		_, err := store.update(ctx, "report", "a", time.Hour, func(r circuitbreaker.Record, _ time.Time) circuitbreaker.Record {
			attempts++
			_, _ = c.UpdateOne(ctx, bson.D{{Key: "_id", Value: store.id("a")}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "version", Value: 1}}}})
			return r
		})
		if !errors.Is(err, faultvane.ErrStoreUnavailable) || !errors.Is(err, errContention) || attempts != maxAttempts {
			t.Fatalf("update() error = %v after %d attempts, want a contention error after %d", err, attempts, maxAttempts)
		}
	})

	t.Run("RF-2 an operation whose context ends before its write changes nothing", func(t *testing.T) {
		store := newStore(t, c)
		limited, cancel := context.WithCancel(ctx)
		_, err := store.update(limited, "report", "a", time.Hour, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
			cancel()
			return r.Report(now, report(5, time.Hour))
		})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("update() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.State != circuitbreaker.Closed || snapshot.Generation != 0 {
			t.Fatalf("Load() = %+v, %v, want closed generation 0", snapshot, err)
		}
	})

	t.Run("GEN-2 many first operations on new breakers at the same time all succeed", func(t *testing.T) {
		store := newStore(t, c)
		var failed atomic.Int64
		var group sync.WaitGroup
		for i := range 64 {
			group.Add(1)
			go func() {
				defer group.Done()
				if _, err := store.Report(ctx, fmt.Sprintf("new-%d", i%8), report(0, time.Hour)); err != nil {
					failed.Add(1)
				}
			}()
		}
		group.Wait()
		if failed.Load() != 0 {
			t.Fatalf("%d first operations failed", failed.Load())
		}
	})

	t.Run("GEN-2 an operation that loses one race applies its change to the new state", func(t *testing.T) {
		store := newStore(t, c)
		attempts := 0
		snapshot, err := store.update(ctx, "report", "a", time.Hour, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
			attempts++
			if attempts == 1 {
				if _, err := store.Report(ctx, "a", report(3, time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			return r.Report(now, report(2, time.Hour))
		})
		if err != nil || attempts != 2 || snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("update() = %+v, %v after %d attempts, want open generation 1 after 2", snapshot, err, attempts)
		}
	})
}

func TestFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("RF-1 ERR-3 each operation on a MongoDB that does not answer returns ErrStoreUnavailable", func(t *testing.T) {
		store := newStore(t, connect(t, "mongodb://127.0.0.1:1"))
		_, loadErr := store.Load(ctx, "a")
		_, reportErr := store.Report(ctx, "a", report(1, time.Hour))
		grant, acquireErr := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 1, Limit: 1})
		_, completeErr := store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Token: 1})
		_, restoreErr := store.Restore(ctx, "a", circuitbreaker.Snapshot{Generation: 1})
		failures := map[string]error{"load": loadErr, "report": reportErr, "acquire probe": acquireErr, "complete probe": completeErr, "restore": restoreErr, "ensure indexes": store.EnsureIndexes(ctx)}
		for operation, err := range failures {
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "mongostore: "+operation+": ") {
				t.Errorf("%s: error = %v", operation, err)
			}
		}
		if grant.Granted {
			t.Error("a failed probe request is granted")
		}
	})

	t.Run("RF-2 API-3 an operation stops when its context ends", func(t *testing.T) {
		store := newStore(t, collection(t))
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Report(cancelled, "a", report(5, time.Hour)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Report() error = %v, want context.Canceled", err)
		}
		if _, err := store.Load(cancelled, "a"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want context.Canceled", err)
		}
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.Generation != 0 {
			t.Fatalf("Load() = %+v, %v", snapshot, err)
		}
	})
}

func newBreakers(t *testing.T, store *Store, count int, opts ...circuitbreaker.Option) []*circuitbreaker.Breaker {
	t.Helper()
	breakers := make([]*circuitbreaker.Breaker, count)
	for i := range breakers {
		base := []circuitbreaker.Option{
			circuitbreaker.WithStore(store), circuitbreaker.WithLogger(quiet),
			circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithMinimumCalls(1000),
			circuitbreaker.WithWindowSize(10 * time.Minute), circuitbreaker.WithOpenDuration(500 * time.Millisecond),
			circuitbreaker.WithSyncInterval(100 * time.Millisecond), circuitbreaker.WithStoreTimeout(90 * time.Millisecond),
			circuitbreaker.WithEarlyReportInterval(10 * time.Millisecond), circuitbreaker.WithFaultThreshold(1000),
			circuitbreaker.WithProbeAttemptInterval(0), circuitbreaker.WithStartTimeout(5 * time.Second),
		}
		breaker, err := circuitbreaker.New("dependency", append(base, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = breaker.Close(context.Background()) })
		breakers[i] = breaker
	}
	return breakers
}

func all(breakers []*circuitbreaker.Breaker, state circuitbreaker.State) func() bool {
	return func() bool {
		for _, b := range breakers {
			if b.State() != state || b.Mode() != circuitbreaker.Distributed {
				return false
			}
		}
		return true
	}
}

func TestAcceptance(t *testing.T) {
	ctx := context.Background()
	c := collection(t)
	fail := func(context.Context) error { return errDependency }

	t.Run("AT-1 FD-6 ten instances with one failure each open the circuit through MongoDB", func(t *testing.T) {
		store := newStore(t, c)
		breakers := newBreakers(t, store, 10, circuitbreaker.WithOpenDuration(time.Hour))
		for _, b := range breakers {
			_ = b.Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		if snapshot, err := store.Load(ctx, "dependency"); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("MongoDB has %+v, %v, want open generation 1", snapshot, err)
		}
	})

	t.Run("AT-2 HO-1 exactly one of ten instances gets the probe through MongoDB", func(t *testing.T) {
		store := newStore(t, c)
		breakers := newBreakers(t, store, 10, circuitbreaker.WithProbeTimeout(time.Hour))
		for range 5 {
			_ = breakers[0].Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		time.Sleep(600 * time.Millisecond)
		var permits []circuitbreaker.Permit
		var mu sync.Mutex
		var group sync.WaitGroup
		for _, b := range breakers {
			group.Add(1)
			go func() {
				defer group.Done()
				if permit, err := b.Allow(ctx); err == nil {
					mu.Lock()
					permits = append(permits, permit)
					mu.Unlock()
				}
			}()
		}
		group.Wait()
		if len(permits) != 1 {
			t.Fatalf("%d instances got a probe, want 1", len(permits))
		}
		permits[0].Success()
		eventually(t, "all instances are closed", all(breakers, circuitbreaker.Closed))
		if snapshot, _ := store.Load(ctx, "dependency"); snapshot.Generation != 3 {
			t.Fatalf("MongoDB has generation %d, want 3", snapshot.Generation)
		}
	})

	t.Run("RF-5 an instance restores the state after MongoDB lost the document", func(t *testing.T) {
		store := newStore(t, c)
		breakers := newBreakers(t, store, 2, circuitbreaker.WithOpenDuration(time.Hour))
		for range 5 {
			_ = breakers[0].Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		if _, err := c.DeleteOne(ctx, bson.D{{Key: "_id", Value: store.id("dependency")}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "MongoDB has the open state again", func() bool {
			snapshot, err := store.Load(ctx, "dependency")
			return err == nil && snapshot.State == circuitbreaker.Open && snapshot.Generation == 1
		})
	})
}

func BenchmarkOperations(b *testing.B) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri()))
	if err != nil {
		b.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	store, err := New(client.Database("faultvane_test").Collection("circuit_breakers"), WithNamespace(fmt.Sprintf("bench-%d", time.Now().UnixNano())))
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Report", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.Report(context.Background(), "a", report(0, time.Hour)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Load", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.Load(context.Background(), "a"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
