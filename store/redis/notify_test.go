package redisstore

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/circuitbreaker/storetest"
	"github.com/redis/go-redis/v9"
)

const lastMessage = "end"

func newNotifyingStore(t *testing.T, client redis.Scripter, subscriber Subscriber) *Store {
	t.Helper()
	store, err := New(client, WithNamespace(newStore(t, client).namespace), WithNotifications(subscriber))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newNotifiedBreaker(t *testing.T, store circuitbreaker.Store, opts ...circuitbreaker.Option) *circuitbreaker.Breaker {
	t.Helper()
	base := []circuitbreaker.Option{
		circuitbreaker.WithStore(store), circuitbreaker.WithLogger(quiet),
		circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithMinimumCalls(1000),
		circuitbreaker.WithWindowSize(10 * time.Minute), circuitbreaker.WithOpenDuration(300 * time.Millisecond),
		circuitbreaker.WithSyncInterval(time.Minute), circuitbreaker.WithStoreTimeout(2 * time.Second),
		circuitbreaker.WithEarlyReportInterval(5 * time.Millisecond), circuitbreaker.WithProbeAttemptInterval(5 * time.Millisecond),
		circuitbreaker.WithStartTimeout(2 * time.Second),
	}
	breaker, err := circuitbreaker.New("dependency", append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = breaker.Close(context.Background()) })
	return breaker
}

func subscribers(client redis.Cmdable, store *Store) func() int64 {
	channel := store.channel("dependency")
	return func() int64 { return client.PubSubNumSub(context.Background(), channel).Val()[channel] }
}

func waitForSubscribers(t *testing.T, client redis.Cmdable, store *Store, want int64) {
	t.Helper()
	count := subscribers(client, store)
	eventually(t, fmt.Sprintf("the channel has %d subscribers", want), func() bool { return count() == want })
}

func listen(t *testing.T, client *redis.Client, channel string) <-chan *redis.Message {
	t.Helper()
	subscription := client.Subscribe(context.Background(), channel)
	t.Cleanup(func() { _ = subscription.Close() })
	if _, err := subscription.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	return subscription.Channel()
}

func messagesBeforeTheLast(t *testing.T, client *redis.Client, channel string, messages <-chan *redis.Message) []string {
	t.Helper()
	if err := client.Publish(context.Background(), channel, lastMessage).Err(); err != nil {
		t.Fatal(err)
	}
	var payloads []string
	limit := time.After(15 * time.Second)
	for {
		select {
		case message := <-messages:
			if message.Payload == lastMessage {
				return payloads
			}
			payloads = append(payloads, message.Payload)
		case <-limit:
			t.Fatalf("timeout: the last message did not arrive after %v", payloads)
		}
	}
}

func makeEachTransition(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	must := func(_ circuitbreaker.Snapshot, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(token uint64, want bool) {
		t.Helper()
		grant, err := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: token, Limit: 1, LeaseDuration: time.Minute, StateTTL: time.Hour})
		if err != nil || grant.Granted != want {
			t.Fatalf("AcquireProbe(%d) = %+v, %v, want granted %v", token, grant, err, want)
		}
	}
	complete := func(generation, token uint64, result circuitbreaker.Result) {
		t.Helper()
		must(store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Generation: generation, Token: token, Result: result, SuccessThreshold: 1, OpenDuration: time.Hour, StateTTL: time.Hour}))
	}
	ended := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 5, TransitionAt: time.Now().Add(-time.Hour), OpenDuration: time.Second}
	must(store.Load(ctx, "a"))
	must(store.Report(ctx, "a", report(0, 2, time.Hour)))
	must(store.Report(ctx, "a", report(0, 3, time.Hour)))
	must(store.Report(ctx, "a", report(0, 5, time.Hour)))
	must(store.Restore(ctx, "a", ended))
	must(store.Restore(ctx, "a", ended))
	acquire(1, true)
	acquire(2, false)
	complete(6, 1, circuitbreaker.Failure)
	must(store.Restore(ctx, "a", circuitbreaker.Snapshot{State: circuitbreaker.HalfOpen, Generation: 9, TransitionAt: time.Now()}))
	acquire(3, true)
	complete(9, 3, circuitbreaker.Success)
	if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.State != circuitbreaker.Closed || snapshot.Generation != 10 {
		t.Fatalf("Load() = %+v, %v, want closed generation 10", snapshot, err)
	}
}

func TestNotificationConformance(t *testing.T) {
	t.Run("QA-8 DS-6 DS-10 standalone Redis with notifications", func(t *testing.T) {
		client := reachable(t, standalone())
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newNotifyingStore(t, client, client) })
	})

	t.Run("QA-8 KEY-3 DS-10 Redis with cluster mode, a cluster client, and notifications", func(t *testing.T) {
		client := clusterClient(t)
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newNotifyingStore(t, client, client) })
	})
}

func TestNotificationData(t *testing.T) {
	client := reachable(t, standalone())
	ctx := context.Background()

	t.Run("CFG-3 notifications without a subscriber", func(t *testing.T) {
		if store, err := New(client, WithNamespace("a"), WithNotifications(nil)); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
			t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", store, err)
		}
	})

	t.Run("KEY-3 the channel name is deterministic and has the hash tag of the keys", func(t *testing.T) {
		store, err := New(client, WithNamespace("production"), WithNotifications(client))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := store.channel("payment-service"), "faultvane:{production:payment-service}:cb:transitions"; got != want {
			t.Fatalf("channel = %q, want %q", got, want)
		}
		clustered := clusterClient(t)
		slot, err := clustered.ClusterKeySlot(ctx, store.channel("payment-service")).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range store.keys("payment-service") {
			if got, err := clustered.ClusterKeySlot(ctx, key).Result(); err != nil || got != slot {
				t.Fatalf("slot of %s = %d, %v, want the slot %d of the channel", key, got, err, slot)
			}
		}
	})

	t.Run("DS-10 each change of the generation publishes one message with the generation", func(t *testing.T) {
		store := newNotifyingStore(t, client, client)
		messages := listen(t, client, store.channel("a"))
		makeEachTransition(t, store)
		want := []string{"1", "5", "6", "7", "9", "10"}
		if got := messagesBeforeTheLast(t, client, store.channel("a"), messages); !slices.Equal(got, want) {
			t.Fatalf("messages = %v, want %v", got, want)
		}
	})

	t.Run("DS-10 a store without the option publishes no message and subscribes to nothing", func(t *testing.T) {
		store := newStore(t, client)
		channel := store.channel("a")
		messages := listen(t, client, channel)
		stop, err := store.Subscribe(ctx, "a", func() { t.Error("a store without the option called the function") })
		if err != nil {
			t.Fatal(err)
		}
		makeEachTransition(t, store)
		if got := messagesBeforeTheLast(t, client, channel, messages); len(got) != 0 {
			t.Fatalf("messages = %v, want none", got)
		}
		if got := client.PubSubNumSub(ctx, channel).Val()[channel]; got != 1 {
			t.Fatalf("the channel has %d subscribers, want only the 1 of the test", got)
		}
		stop()
	})

	t.Run("DS-11 RF-6 a message that Redis refuses fails no operation", func(t *testing.T) {
		user := fmt.Sprintf("faultvane-test-%d", time.Now().UnixNano())
		if err := client.Do(ctx, "ACL", "SETUSER", user, "on", ">secret", "~*", "+@all", "resetchannels").Err(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Do(ctx, "ACL", "DELUSER", user).Err() })
		restricted := redis.NewClient(&redis.Options{Addr: standalone(), Username: user, Password: "secret", MaxRetries: -1})
		t.Cleanup(func() { _ = restricted.Close() })
		if err := restricted.Publish(ctx, "faultvane-test", "x").Err(); err == nil {
			t.Fatal("the restricted user can publish")
		}
		store := newNotifyingStore(t, restricted, client)
		messages := listen(t, client, store.channel("a"))
		makeEachTransition(t, store)
		if got := messagesBeforeTheLast(t, client, store.channel("a"), messages); len(got) != 0 {
			t.Fatalf("messages = %v, want none", got)
		}
	})
}

type countedStore struct {
	*Store
	operations atomic.Int64
}

func (s *countedStore) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Load(ctx, name)
}

func (s *countedStore) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Report(ctx, name, report)
}

func observeEachTransition(t *testing.T, writer, reader *circuitbreaker.Breaker) {
	t.Helper()
	for range 5 {
		_ = call(writer, errDependency)
	}
	eventually(t, "the reader is open", func() bool { return reader.State() == circuitbreaker.Open })
	var probe circuitbreaker.Permit
	eventually(t, "the writer gets the probe", func() bool {
		var err error
		probe, err = writer.Allow(context.Background())
		return err == nil
	})
	eventually(t, "the reader is half-open", func() bool { return reader.State() == circuitbreaker.HalfOpen })
	probe.Success()
	eventually(t, "the reader is closed", func() bool { return reader.State() == circuitbreaker.Closed })
	if s := reader.Stats(); s.Notifications != 3 || s.SubscribeFailed || s.Mode != circuitbreaker.Distributed || s.Failures != 0 {
		t.Fatalf("stats of the reader = %+v, want 3 notifications in distributed mode and no call", s)
	}
}

func TestNotificationPropagation(t *testing.T) {
	servers := map[string]func(*testing.T) redis.UniversalClient{
		"standalone Redis": func(t *testing.T) redis.UniversalClient { return reachable(t, standalone()) },
		"Redis with cluster mode and a cluster client": func(t *testing.T) redis.UniversalClient { return clusterClient(t) },
	}
	for server, connect := range servers {
		t.Run("DS-10 an instance observes each transition of a different instance inside a sync interval of 1 minute: "+server, func(t *testing.T) {
			client := connect(t)
			store := newNotifyingStore(t, client, client)
			writer, reader := newNotifiedBreaker(t, store), newNotifiedBreaker(t, store)
			waitForSubscribers(t, client, store, 2)
			observeEachTransition(t, writer, reader)
		})
	}

	t.Run("PF-7 200 transitions give each instance at most one store operation in each early report interval", func(t *testing.T) {
		const interval, transitions = 50 * time.Millisecond, 200
		client := reachable(t, standalone())
		store := newNotifyingStore(t, client, client)
		stores := make([]*countedStore, 5)
		breakers := make([]*circuitbreaker.Breaker, len(stores))
		for i := range stores {
			stores[i] = &countedStore{Store: store}
			breakers[i] = newNotifiedBreaker(t, stores[i], circuitbreaker.WithEarlyReportInterval(interval))
		}
		waitForSubscribers(t, client, store, int64(len(stores)))
		for _, s := range stores {
			s.operations.Store(0)
		}
		started := time.Now()
		for generation := uint64(1); generation <= transitions; generation++ {
			snapshot := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: generation, TransitionAt: time.Now(), OpenDuration: time.Hour}
			if generation == transitions {
				snapshot.State = circuitbreaker.HalfOpen
			}
			if _, err := store.Restore(context.Background(), "dependency", snapshot); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, "each instance has all notifications and the last state", func() bool {
			for _, b := range breakers {
				if s := b.Stats(); s.Notifications != transitions || s.State != circuitbreaker.HalfOpen {
					return false
				}
			}
			return true
		})
		for i, s := range stores {
			operations := s.operations.Load()
			limit := int64(time.Since(started)/interval) + 1
			t.Logf("instance %d: %d store operations for %d transitions in %v", i, operations, transitions, time.Since(started))
			if operations < 1 || operations > limit {
				t.Fatalf("instance %d made %d store operations, want 1 to %d", i, operations, limit)
			}
		}
	})
}

func TestNotificationFaults(t *testing.T) {
	throughProxy := func(t *testing.T) (*proxy, *redis.Client) {
		p := newProxy(t, standalone())
		subscriber := redis.NewClient(&redis.Options{Addr: p.listener.Addr().String(), DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = subscriber.Close() })
		return p, subscriber
	}
	open := func(t *testing.T, store *Store) {
		t.Helper()
		if snapshot, err := store.Report(context.Background(), "dependency", report(0, 5, time.Hour)); err != nil || snapshot.State != circuitbreaker.Open {
			t.Fatalf("Report() = %+v, %v, want open", snapshot, err)
		}
	}

	t.Run("RF-1 DS-10 the subscription works again after its connection stopped", func(t *testing.T) {
		client := reachable(t, standalone())
		p, subscriber := throughProxy(t)
		store := newNotifyingStore(t, client, subscriber)
		reader := newNotifiedBreaker(t, store)
		waitForSubscribers(t, client, store, 1)
		p.stop()
		waitForSubscribers(t, client, store, 0)
		p.start()
		waitForSubscribers(t, client, store, 1)
		open(t, store)
		eventually(t, "the reader is open", func() bool { return reader.State() == circuitbreaker.Open })
		if s := reader.Stats(); s.SubscribeFailed || s.Mode != circuitbreaker.Distributed {
			t.Fatalf("stats of the reader = %+v, want distributed mode and no failed subscription", s)
		}
	})

	t.Run("RF-1 RF-6 a subscription that starts during an outage fails nothing and works when Redis is back", func(t *testing.T) {
		client := reachable(t, standalone())
		p, subscriber := throughProxy(t)
		p.stop()
		store := newNotifyingStore(t, client, subscriber)
		reader := newNotifiedBreaker(t, store)
		if err := call(reader, nil); err != nil {
			t.Fatalf("Do() = %v, want nil", err)
		}
		p.start()
		waitForSubscribers(t, client, store, 1)
		open(t, store)
		eventually(t, "the reader is open", func() bool { return reader.State() == circuitbreaker.Open })
	})

	t.Run("API-4 PF-6 Close ends the subscription and its goroutines", func(t *testing.T) {
		client := reachable(t, standalone())
		store := newNotifyingStore(t, client, client)
		before := runtime.NumGoroutine()
		breaker := newNotifiedBreaker(t, store)
		waitForSubscribers(t, client, store, 1)
		if err := breaker.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForSubscribers(t, client, store, 0)
		eventually(t, "the goroutines of the subscription ended", func() bool { return runtime.NumGoroutine() <= before })
	})
}
