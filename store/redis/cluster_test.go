package redisstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/circuitbreaker/storetest"
	"github.com/faultvane/faultvane-go/ratelimit"
	ratelimittest "github.com/faultvane/faultvane-go/ratelimit/storetest"
	"github.com/redis/go-redis/v9"
)

func clusterNodes() string { return address("FAULTVANE_REDIS_CLUSTER_NODES_ADDR", "127.0.0.1:6391") }

func nodesClient(t *testing.T) *redis.ClusterClient {
	t.Helper()
	client := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{clusterNodes()}, DialTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	eventually(t, "the Redis Cluster at "+clusterNodes()+" answers", func() bool {
		return client.Set(context.Background(), "faultvane-test-ready", 1, time.Second).Err() == nil
	})
	return client
}

func slotRanges(t *testing.T, client *redis.ClusterClient) []redis.ClusterSlot {
	t.Helper()
	shards, err := client.ClusterShards(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	var ranges []redis.ClusterSlot
	for _, shard := range shards {
		primary := slices.IndexFunc(shard.Nodes, func(node redis.Node) bool { return node.Role == "master" })
		if primary < 0 {
			t.Fatalf("the shard %+v has no primary node", shard)
		}
		node := shard.Nodes[primary]
		address := net.JoinHostPort(node.IP, strconv.FormatInt(node.Port, 10))
		for _, slots := range shard.Slots {
			ranges = append(ranges, redis.ClusterSlot{Start: int(slots.Start), End: int(slots.End), Nodes: []redis.ClusterNode{{Addr: address}}})
		}
	}
	return ranges
}

func primaries(t *testing.T, client *redis.ClusterClient) []string {
	t.Helper()
	var nodes []string
	for _, slots := range slotRanges(t, client) {
		if !slices.Contains(nodes, slots.Nodes[0].Addr) {
			nodes = append(nodes, slots.Nodes[0].Addr)
		}
	}
	slices.Sort(nodes)
	if len(nodes) < 3 {
		t.Fatalf("the cluster has the primary nodes %v, want at least 3", nodes)
	}
	return nodes
}

func owner(t *testing.T, client *redis.ClusterClient, key string) string {
	t.Helper()
	slot, err := client.ClusterKeySlot(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, slots := range slotRanges(t, client) {
		if int64(slots.Start) <= slot && slot <= int64(slots.End) {
			return slots.Nodes[0].Addr
		}
	}
	t.Fatalf("no node has the slot %d of %s", slot, key)
	return ""
}

func storeOn(t *testing.T, client *redis.ClusterClient, node string, opts ...Option) *Store {
	t.Helper()
	for range 1000 {
		store, err := New(client, append([]Option{WithNamespace(newStore(t, client).namespace)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		if owner(t, client, store.keys("dependency")[0]) == node {
			return store
		}
	}
	t.Fatalf("no namespace puts the breaker on %s", node)
	return nil
}

func onlyOn(t *testing.T, nodes []string, node, key string) {
	t.Helper()
	for _, other := range nodes {
		count, err := newClient(t, other).Exists(context.Background(), key).Result()
		if moved, ok := redis.IsMovedError(err); other == node && count != 1 || other != node && (!ok || moved != node) {
			t.Fatalf("EXISTS %s on %s = %d, %v, want the key only on %s", key, other, count, err, node)
		}
	}
}

func moveSlot(t *testing.T, nodes []string, slot int64, from, to string) {
	t.Helper()
	ctx := context.Background()
	source, target := newClient(t, from), newClient(t, to)
	must := func(command *redis.Cmd) {
		t.Helper()
		if err := command.Err(); err != nil {
			t.Fatalf("%v: %v", command.Args(), err)
		}
	}
	must(target.Do(ctx, "CLUSTER", "SETSLOT", slot, "IMPORTING", source.ClusterMyID(ctx).Val()))
	must(source.Do(ctx, "CLUSTER", "SETSLOT", slot, "MIGRATING", target.ClusterMyID(ctx).Val()))
	host, port, _ := net.SplitHostPort(to)
	migrate := []any{"MIGRATE", host, port, "", 0, 5000, "KEYS"}
	for _, key := range source.ClusterGetKeysInSlot(ctx, int(slot), 1000).Val() {
		migrate = append(migrate, key)
	}
	if len(migrate) > 7 {
		must(source.Do(ctx, migrate...))
	}
	for _, node := range append([]string{to}, nodes...) {
		must(newClient(t, node).Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", target.ClusterMyID(ctx).Val()))
	}
}

func TestClusterNodes(t *testing.T) {
	client := nodesClient(t)
	nodes := primaries(t, client)
	ctx := context.Background()

	t.Run("QA-8 KEY-2 DS-6 the circuit breaker store on a Redis Cluster of 3 nodes", func(t *testing.T) {
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newStore(t, client) })
	})

	t.Run("QA-8 KEY-3 DS-10 the circuit breaker store with notifications on a Redis Cluster of 3 nodes", func(t *testing.T) {
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newNotifyingStore(t, client, client) })
	})

	t.Run("RL-21 RL-37 QA-8 the rate limit store on a Redis Cluster of 3 nodes", func(t *testing.T) {
		ratelimittest.Run(t, func(t *testing.T) ratelimit.Store { return rateLimitStore(t, client) })
	})

	t.Run("KEY-2 KEY-3 RL-37 the breakers and the rate limits of one namespace are on each node and stay separate", func(t *testing.T) {
		store := newNotifyingStore(t, client, client)
		limits, err := NewRateLimitStore(client, WithNamespace(store.namespace))
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]string{}
		for i := 0; len(names) < len(nodes) && i < 1000; i++ {
			name := fmt.Sprintf("dependency-%d", i)
			if node := owner(t, client, store.keys(name)[0]); names[node] == "" {
				names[node] = name
			}
		}
		if len(names) != len(nodes) {
			t.Fatalf("the names %v are not on each of the nodes %v", names, nodes)
		}
		for node, name := range names {
			for _, key := range append(store.keys(name), store.channel(name), limits.key(name)) {
				if got := owner(t, client, key); got != node {
					t.Fatalf("%s is on %s, want %s with the other keys of %s", key, got, node, name)
				}
			}
			failures := int64(2)
			if node == nodes[0] {
				failures = 5
			}
			if _, err := store.Report(ctx, name, report(0, failures, time.Hour)); err != nil {
				t.Fatal(err)
			}
			if grant, err := limits.Acquire(ctx, name, ratelimit.Acquire{Wanted: failures, Rate: 1e-9, Burst: 5, StateTTL: time.Hour}); err != nil || grant.Tokens != failures {
				t.Fatalf("Acquire(%s) = %+v, %v, want %d tokens", name, grant, err, failures)
			}
		}
		for node, name := range names {
			want, tokens := circuitbreaker.Snapshot{State: circuitbreaker.Closed}, int64(3)
			if node == nodes[0] {
				want, tokens = circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 1}, 0
			}
			if got, err := store.Load(ctx, name); err != nil || got.State != want.State || got.Generation != want.Generation {
				t.Fatalf("Load(%s) on %s = %+v, %v, want %+v", name, node, got, err, want)
			}
			if grant, err := limits.Acquire(ctx, name, ratelimit.Acquire{Wanted: 5, Rate: 1e-9, Burst: 5, StateTTL: time.Hour}); err != nil || grant.Tokens != tokens {
				t.Fatalf("Acquire(%s) on %s = %+v, %v, want the %d tokens that stay", name, node, grant, err, tokens)
			}
			onlyOn(t, nodes, node, store.keys(name)[0])
			onlyOn(t, nodes, node, limits.key(name))
		}
	})

	for _, node := range nodes {
		t.Run("AT-1 FD-6 ten instances with one failure each open the circuit of a breaker on the node "+node, func(t *testing.T) {
			direct := storeOn(t, client, node)
			instances := make([]instance, 10)
			for i := range instances {
				store, err := New(nodesClient(t), WithNamespace(direct.namespace))
				if err != nil {
					t.Fatal(err)
				}
				instances[i].breaker = newNotifiedBreaker(t, store, circuitbreaker.WithOpenDuration(time.Hour), circuitbreaker.WithSyncInterval(time.Second), circuitbreaker.WithStoreTimeout(500*time.Millisecond))
			}
			var reached atomic.Int64
			for _, i := range instances {
				_ = i.breaker.Do(ctx, func(context.Context) error {
					reached.Add(1)
					return errDependency
				})
			}
			eventually(t, "all instances are open", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
			if snapshot := stored(t, direct); snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
				t.Fatalf("Redis has %+v, want open generation 1", snapshot)
			}
			onlyOn(t, nodes, node, direct.keys("dependency")[0])
			for _, i := range instances {
				if err := call(i.breaker, nil); !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
					t.Fatalf("Do() = %v, want ErrCircuitOpen", err)
				}
				if failures := i.breaker.Stats().Failures; failures != 1 {
					t.Fatalf("an instance has %d failures, want 1", failures)
				}
			}
			if reached.Load() != 10 {
				t.Fatalf("the dependency got %d calls, want 10", reached.Load())
			}
		})
	}

	propagates := func(t *testing.T, publisher, subscriber string, connection Subscriber) {
		t.Helper()
		store := storeOn(t, client, publisher, WithNotifications(connection))
		writer, reader := newNotifiedBreaker(t, store), newNotifiedBreaker(t, store)
		waitForSubscribers(t, newClient(t, subscriber), store, 2)
		for _, node := range nodes {
			if count := subscribers(newClient(t, node), store)(); node != subscriber && count != 0 {
				t.Fatalf("the node %s has %d subscribers, want all subscribers on %s", node, count, subscriber)
			}
		}
		observeEachTransition(t, writer, reader)
		onlyOn(t, nodes, publisher, store.keys("dependency")[0])
	}

	for _, node := range nodes {
		t.Run("DS-10 a cluster client subscribes on the node of the breaker and observes each transition: "+node, func(t *testing.T) {
			propagates(t, node, node, client)
		})
		for _, subscriber := range nodes {
			if subscriber != node {
				t.Run(fmt.Sprintf("DS-10 each transition of a breaker on the node %s reaches a subscriber on the node %s", node, subscriber), func(t *testing.T) {
					propagates(t, node, subscriber, newClient(t, subscriber))
				})
			}
		}
	}

	t.Run("KEY-2 DS-10 a breaker keeps its window and its notifications after its slot moves to a different node", func(t *testing.T) {
		from, to := nodes[0], nodes[1]
		store := storeOn(t, client, from, WithNotifications(client))
		reader := newNotifiedBreaker(t, store)
		waitForSubscribers(t, newClient(t, from), store, 1)
		if _, err := store.Report(ctx, "dependency", report(0, 2, time.Hour)); err != nil {
			t.Fatal(err)
		}
		slot := client.ClusterKeySlot(ctx, store.channel("dependency")).Val()
		moveSlot(t, nodes, slot, from, to)
		t.Cleanup(func() {
			if err := client.Del(ctx, store.keys("dependency")...).Err(); err != nil {
				t.Error(err)
			}
			moveSlot(t, nodes, slot, to, from)
		})
		onlyOn(t, nodes, to, store.keys("dependency")[1])
		if snapshot, err := store.Report(ctx, "dependency", report(0, 3, time.Hour)); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("Report() after the move = %+v, %v, want open generation 1 from 2 failures before and 3 after the move", snapshot, err)
		}
		eventually(t, "the reader is open", func() bool { return reader.State() == circuitbreaker.Open })
		if s := reader.Stats(); s.Notifications != 1 || s.Mode != circuitbreaker.Distributed {
			t.Fatalf("stats of the reader = %+v, want 1 notification in distributed mode", s)
		}
	})
}
