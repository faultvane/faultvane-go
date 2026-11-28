# Redis adapter

This document describes the Redis adapter.
The [store contract](README.md) defines the rules for all adapters.
This document defines how the Redis adapter satisfies them: the keys, the fields, the atomic scripts, and the TTL rules.
After version 1.0, this data model is a compatibility commitment.

| Item | Value |
|---|---|
| Module | `github.com/faultvane/faultvane-go/store/redis` |
| Package name | `redisstore` |
| Minimum Go version | 1.26. The Redis client sets this version. |
| Client | `github.com/redis/go-redis/v9` |

## 1. Setup

```go
client := redis.NewClient(&redis.Options{Addr: "redis.internal:6379"})

store, err := redisstore.New(client, redisstore.WithNamespace("production"))
if err != nil {
    return err
}

breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(store))
if err != nil {
    return err
}
defer breaker.Close(ctx)
```

`New` accepts each client of `go-redis` that can run scripts. Its parameter has the type `redis.Scripter`.
`New` returns an error that matches `faultvane.ErrInvalidConfig` in these conditions:

- The client is `nil`.
- The namespace is not set, or it is not valid.
- The key prefix is not valid.
- The subscriber of `WithNotifications` is `nil`.

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names, for example `production`. |
| `WithKeyPrefix(prefix)` | `faultvane` | The first part of each key. |
| `WithNotifications(subscriber)` | Off | Transition notifications with Redis Pub/Sub. See section 8. |

## 2. Keys

Each breaker uses four keys:

| Key | Type | Content |
|---|---|---|
| `<prefix>:{<namespace>:<name>}:cb:state` | Hash | The authoritative state |
| `<prefix>:{<namespace>:<name>}:cb:window` | Hash | The shared window |
| `<prefix>:{<namespace>:<name>}:cb:probes` | Sorted set | The probe leases |
| `<prefix>:{<namespace>:<name>}:cb:reporters` | Hash | The last report of each instance |

Example with the default prefix:

```text
faultvane:{production:payment-service}:cb:state
faultvane:{production:payment-service}:cb:window
faultvane:{production:payment-service}:cb:probes
faultvane:{production:payment-service}:cb:reporters
```

Rules:

- The default prefix is `faultvane`. It is configurable.
- The namespace separates environments and groups of services. Instances that use the same namespace and name share one breaker.
- The part in braces is a Redis Cluster hash tag. The four keys of one breaker are always in the same slot. A script can then use all four keys.
- The prefix, the namespace, and the name accept only the characters `a-z`, `A-Z`, `0-9`, `.`, `_`, and `-`. The maximum length of each is 64. This rule prevents collisions, because the separator `:` and the braces cannot occur in a name.
- `cb` identifies the circuit breaker.

The rate limit store of this module uses one more key for each name: `<prefix>:{<namespace>:<name>}:rl:bucket`. See the [rate limit store](../ratelimit/store.md).

## 3. Units

| Value | Unit |
|---|---|
| Times in the state hash and in the leases | Microseconds since the Unix epoch, from the Redis command `TIME` |
| Durations in the state hash and in the script arguments | Microseconds |
| TTL of the keys | Milliseconds, set with `PEXPIRE` |

A duration above 0 and below 1 microsecond becomes 1 microsecond.
A TTL below 1 millisecond becomes 1 millisecond.
A script uses Lua numbers. A generation or a time keeps its exact value up to 2^53.

## 4. State hash

| Field | Type | Meaning |
|---|---|---|
| `v` | Integer | Schema version. The value is `1`. |
| `st` | Integer | State: `0` `CLOSED`, `1` `OPEN`, `2` `HALF_OPEN`. |
| `gen` | Integer | Generation. |
| `ts` | Integer | Redis time of the last transition, in microseconds. |
| `od` | Integer | Open duration of the current `OPEN` state, in microseconds. |
| `ps` | Integer | Number of probe successes in the current `HALF_OPEN` state. |
| `fp` | Text | Configuration fingerprint of the last writer. |

A key that does not exist means `CLOSED` with generation 0.
A hash without the field `fp` is valid. Its fingerprint is the empty text. A hash that an older version of the scripts wrote has no field `fp`.
The schema version stays `1`, because the new field changes no existing field.

## 5. Window hash

For each bucket, the hash has three fields:

| Field | Meaning |
|---|---|
| `<bucket>:c` | Calls |
| `<bucket>:f` | Failures |
| `<bucket>:s` | Slow calls |

`<bucket>` is the Redis time in microseconds divided by the bucket duration in microseconds, as an integer.
The report script deletes the fields of buckets that are older than the window size.

## 6. Probes sorted set

| Part | Meaning |
|---|---|
| Member | The lease token. It is a random 64-bit number that the instance creates. The member is its decimal text. |
| Score | The Redis time at which the lease ends, in microseconds. |

A lease is valid when its score is larger than the current Redis time.

## 7. Scripts

Each operation runs in one Lua script.
Redis runs a script atomically.
The scripts are in the directory `store/redis/scripts`. The Go compiler embeds them in the binary.
The file `common.lua` contains the functions that all scripts use. The adapter puts it before each script.

Each script reads the time with `TIME` and returns the authoritative state after its work:

```text
(state, generation, transitionTime, openDuration, redisTime, granted, previousFingerprint)
```

`granted` is `1` only when `acquire_probe` grants a lease.
`previousFingerprint` is the value of the field `fp` before the script changed it.

| Script | Store operation | Arguments | Behavior |
|---|---|---|---|
| `load` | `Load` | None | Reads the state hash. Changes nothing. |
| `report` | `Report` | generation, calls, failures, slow calls, failure count threshold, minimum calls, failure rate, slow call rate, bucket duration, number of buckets, open duration, TTL, fingerprint | Does nothing when the generation is different or the state is not `CLOSED`. Deletes the old buckets. Adds the counts to the current bucket. Evaluates the failure condition. Makes transition T1 when the condition is true. Saves the fingerprint. |
| `acquire_probe` | `AcquireProbe` | token, probe limit, lease duration, TTL, fingerprint | Makes transition T2 when the state is `OPEN` and the open duration ended. In `HALF_OPEN`, removes the leases that ended, then adds the token when the number of valid leases is below the limit, and saves the fingerprint. Returns if the lease is granted. |
| `complete_probe` | `CompleteProbe` | generation, token, result, required successes, open duration, TTL, fingerprint | Does nothing when the state is not `HALF_OPEN` or the generation is different. Saves the fingerprint. Changes no state when the token has no valid lease. Removes the lease. On failure, makes transition T4. On success, increments `ps` and makes transition T3 when `ps` reaches the required number. |
| `restore` | `Restore` | state, generation, transition time, open duration, TTL | Writes the snapshot only when its generation is higher than the stored generation. A key that does not exist has generation 0. Deletes the window and the leases when it writes. Keeps the field `fp`. |

Each transition deletes the window and the leases, and sets `ps` to 0. A transition keeps the field `fp`.
With the option `WithNotifications`, each transition and each `restore` that writes also publish one message. Section 8 describes it.
The [state machine](../circuit-breaker/state-machine.md) defines the transitions T1 to T4.

### Second implementation of the transitions

The scripts are a second implementation of the transitions. The first is the Go type `circuitbreaker.Record`.
The conformance suite runs the same random sequences of operations on the adapter and on the in-memory store, and compares the results.
[ADR 0007](../adr/0007-shared-record-for-go-adapters.md) records this decision.

### Script loading

The Redis client calls a script with `EVALSHA`.
When Redis answers `NOSCRIPT`, the client sends the script with `EVAL`.
This also handles a Redis restart and a failover.

### Optimistic concurrency

The generation gives the same result as a compare-and-set:

- A `report` with an old generation does not change the window or the state.
- A `complete_probe` with an old generation or an old token does not change the state.
- A `restore` with a generation that is not higher does not change the state.

Example for acceptance test AT-5:

```text
Redis: generation 100, CLOSED
Instance A reads generation 100
Instance B: report → T1 → generation 101, OPEN
Instance A: report(generation 100) → ignored, response: OPEN, generation 101
```

## 8. Transition notifications

Without this function, an instance observes a transition of a different instance at its next synchronization, at most one sync interval later.
With this function, Redis tells each instance about a transition immediately. The instance then synchronizes immediately.
[Architecture](../circuit-breaker/architecture.md) gives the rules of the core. [ADR 0009](../adr/0009-transition-notifications.md) records the decision.

The function is off by default. The option `WithNotifications` sets it on:

```go
client := redis.NewClient(&redis.Options{Addr: "redis.internal:6379"})

store, err := redisstore.New(client,
    redisstore.WithNamespace("production"),
    redisstore.WithNotifications(client),
)
```

The option is necessary because `New` accepts a `redis.Scripter`, and a `redis.Scripter` cannot subscribe.
The parameter of the option has the type `redisstore.Subscriber`:

```go
type Subscriber interface {
    Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}
```

The types `*redis.Client`, `*redis.ClusterClient`, and `*redis.Ring` of `go-redis` implement it. The usual value is the client that `New` gets.
The store implements the optional interface `circuitbreaker.Notifier`. The breaker finds it and subscribes. The application calls no other function.
The rate limit store of this module ignores the option.

### Channel and message

Each breaker has one channel:

```text
<prefix>:{<namespace>:<name>}:cb:transitions
```

Example: `faultvane:{production:payment-service}:cb:transitions`.
The channel name obeys the rules of section 2. It has the hash tag of the keys of the breaker.

A script publishes one message when it changes the generation:

| Script | Condition |
|---|---|
| `report` | Transition T1 |
| `acquire_probe` | Transition T2 |
| `complete_probe` | Transition T3 or T4 |
| `restore` | The script writes the snapshot |

The message is the new generation as decimal text, for example `8`.
The message is for a person who examines the channel, for example with `redis-cli`. The breaker does not read it.
A message means only: "the state of this breaker changed, read it".

The channel name is the fifth key of a script call. A script publishes only when it gets this key.
With the option off, the adapter sends four keys, and the scripts publish nothing and work as before.
The scripts publish with `redis.pcall`. When Redis refuses the message, for example because the user has no permission for the channel, the operation is still successful and no message goes out.

### `PUBLISH`, not `SPUBLISH`

The scripts use the command `PUBLISH`.
The sharded command `SPUBLISH` needs Redis 7.0. The minimum version of the adapter is Redis 6.2.
In a Redis Cluster, `PUBLISH` sends each message to all nodes of the cluster. A subscriber can then connect to each node. A cluster client subscribes on the node of the slot of the channel name.
The cost is one message on the cluster bus for each node and each transition. Transitions are rare, so this cost is low.
The hash tag in the channel name keeps a later change to `SPUBLISH` possible, because a sharded channel must be in the slot of the keys of the script.

### Subscription

- Each breaker of each instance has one subscription. A subscription of `go-redis` uses its own connection to Redis. It does not use a connection of the command pool.
- The adapter starts one goroutine for each subscription. The client library starts two more. `Close` of the breaker stops them and closes the connection.
- When the connection stops, the client library connects again and subscribes again. The adapter and the breaker do nothing for this.
- When Redis is not reachable at the start, `Subscribe` returns no error. The client library connects when Redis is reachable again.
- Messages that Redis publishes while the connection is down are lost. The instance then observes the transition at its next synchronization.

### Rules for operation

- Set the option in all instances. An instance without the option publishes no message for its transitions and gets no notification. An older version of the adapter also publishes no message. The sync interval then gives the propagation for these transitions.
- All instances must use the same Redis for the scripts and for the subscription.
- A wrapper around the store must pass the method `Subscribe` to the store. `faultvaneotel.InstrumentStore` passes it.
- With Redis access control lists, the user needs the permission for the channels `<prefix>:*`, for example `&faultvane:*`.
- For each transition, each instance makes one store operation at the same time. Section 13 gives the measured times.

## 9. How the adapter satisfies the contract

| Rule | How |
|---|---|
| C1 Atomic | One Lua script for each operation. |
| C2 Ordered | Redis runs one script at a time. |
| C3 State machine | The scripts make only the transitions T1 to T4. |
| C4 Generation | Each transition adds 1 to `gen`. A script compares the generation of the event with `gen`. |
| C5 One clock | The Redis command `TIME`. Each reply contains this time. |
| C6 Leases | The sorted set. The script removes the leases that ended before it counts the valid leases. |
| C7 Isolation | The namespace and the name are parts of each key. |
| C8 Context | The adapter passes the context to the client. The adapter does not retry. |
| C9 Errors | Each error of the client and each reply that is not valid matches `faultvane.ErrStoreUnavailable`. The message starts with `redisstore:` and the name of the operation. |
| C10 Cleanup | The TTL of the keys. See section 10. |
| C11 Data loss is visible | A key that does not exist gives `CLOSED` with generation 0. |
| C12 No side effects | An operation starts no goroutine and holds no lock. The adapter has no timer. With `WithNotifications`, each subscription of a breaker has one goroutine of the adapter. The request path does not use it. |
| C13 Fingerprint | The field `fp` of the state hash. A script that accepts an event writes the fingerprint of its argument. Each reply contains the value from before the script. |
| C14 Report identity | The reporters hash. The field is the instance number as decimal text. The value is `<bucket>:<sequence>` of the last report that the script accepted from this instance. The report script ignores a report when the bucket of the value is still in the window and the sequence of the value is equal to or higher than the sequence of the report. The first report in a new bucket removes the fields whose bucket left the window. A transition and the restore script delete the key. Each write sets the TTL of the key. A script of version 0.2.0 or older does not use this key. See [ADR 0011](../adr/0011-report-identity.md). |

## 10. TTL

A script that accepts an event sets the same TTL on the four keys.
A script that ignores an event does not change the TTL.

The breaker calculates the TTL from its configuration and sends it with each operation:

```text
effective state TTL = max(StateTTL, 2 × (OpenDuration + WindowSize))
```

The default `StateTTL` is 1 hour.
The breaker does not reject a low `StateTTL`. It uses the effective value.
The `restore` script uses a fixed TTL of 1 hour. The next accepted write sets the effective state TTL again.

| Situation | Behavior |
|---|---|
| A `CLOSED` breaker with traffic | Each accepted report sets the TTL again. The state does not expire. |
| `OPEN` state | The TTL is at least two times the open duration. The state cannot expire before the open duration ends, except after a restore with an open duration above 1 hour. |
| `HALF_OPEN` state | Each probe script sets the TTL again. Each lease also has its own end time. |
| A breaker with no traffic | The keys expire after the TTL. This is the cleanup. |
| An instance still holds a snapshot of an expired state | The instance handles this as data loss. See section 11. |

## 11. Data loss

Redis can lose the state through eviction, a restart without persistence, a failover to a replica that is behind, or an expired TTL.

An instance detects data loss when the stored generation is lower than the generation of its last authoritative snapshot.
A key that does not exist has generation 0.

The instance then calls `Restore` with that snapshot.
Only a snapshot that came from the store is permitted. A state from local mode is never permitted.
If many instances call `Restore`, the highest generation wins.
All instances then converge to that state.

Known limit: after a failover that loses a transition, a new transition can get a generation number that existed before with other content.
Each response contains the full state, and an instance always replaces its snapshot with the response.
The effect is limited to results that are in progress at that time. Probe tokens of the lost state are not valid in the new state.

## 12. Redis deployment

| Topic | Rule |
|---|---|
| Version | Redis 6.2 or later, or a compatible server. The tests of the project run with Redis 7 only. |
| Topology | Standalone and Cluster. The hash tag makes all scripts single-slot. The tests run against one standalone server, against one server with cluster mode on, and against a Redis Cluster of 3 primary nodes. See "Tests with a Redis Cluster" below. The project has no test with Sentinel, no test with replicas, and no test of a failover. |
| Eviction | Use a policy that does not evict these keys, or accept that eviction is handled as data loss. |
| Persistence | Not necessary. Section 11 covers the loss of data. |
| Security | TLS and authentication are settings of the Redis client that the application supplies. |

### Tests with a Redis Cluster

The tests in `store/redis/cluster_test.go` use a Redis Cluster of 3 primary nodes without replicas.
The service `redis-cluster-nodes` of `docker-compose.test.yml` starts it. One container runs three `redis-server` processes on the ports 6391, 6392, and 6393.
Each node announces the address `127.0.0.1` and its port. Docker publishes each port with the same number.
The nodes are in one container, so they reach each other on `127.0.0.1`. The tests run on the host and reach the nodes on the same addresses through the published ports.
For this reason, the addresses that the cluster gives to a client are correct on Linux and on macOS with Docker Desktop.
The container makes the cluster when it starts. Its health check passes when each node reports `cluster_state:ok`.

The tests connect with a `redis.ClusterClient` that gets the slot map from the cluster.
The variable `FAULTVANE_REDIS_CLUSTER_NODES_ADDR` sets the address of the first node. The default is `127.0.0.1:6391`.
A test fails when the cluster does not answer in 15 seconds. No test skips.
The tests find the node of a key with `CLUSTER KEYSLOT` and `CLUSTER SLOTS`. They then send `EXISTS` to each node directly: the node of the key finds the key, and each other node answers `MOVED`.

The tests show these facts:

- The conformance suite of the circuit breaker store passes, with and without notifications.
- The conformance suite of the rate limit store passes.
- Breakers and rate limits of one namespace on three different nodes keep separate states. All keys and the channel of one name are on one node.
- Ten instances, each with its own cluster client, open the circuit of a breaker together (acceptance test AT-1). The test runs one time for a breaker on each node.
- A cluster client subscribes on the node of the breaker. The command `PUBSUB NUMSUB` on each node shows this.
- A notification goes from the node of the breaker to a subscriber on a different node. The test uses each of the 6 pairs of different nodes. The subscriber is a `*redis.Client` that connects to one node only.
- A breaker keeps its window after its slot moves to a different node with `CLUSTER SETSLOT` and `MIGRATE`. The cluster client follows the `MOVED` answer. A subscription that started on the old node still gets the notification.

The tests do not show the behavior during a slot move with keys on both nodes, with a node that stops, or with a failover.

## 13. Measured latency

| Operation | Time for each operation |
|---|---|
| `Report` | 0.13 ms |

The measurement used one client and sequential operations, with Redis in a local container.
[Benchmarks](../benchmarks.md) gives the machine and the method.
A network between the application and Redis adds its round-trip time.

### Propagation time of a transition

The propagation time is the time from the end of the store operation that makes a transition to the moment when a different instance shows the new state.

| Configuration | Median | Percentile 90 | Maximum |
|---|---|---|---|
| With `WithNotifications` | 0.9 ms | 1.2 ms | 43 ms |
| Without notifications | 300 ms | 470 ms | 490 ms |

The measurement used one instance with the default options (sync interval 500 ms, early report interval 50 ms), an Apple M4 processor, and Redis 7.4 in a local container, on 2026-11-24.
Each row has two runs of 40 transitions, with a random time of 100 ms to 700 ms between two transitions. The table gives the less favorable value of the two runs.
The maximum with notifications occurs when the instance synchronized a short time before the notification. The instance then waits for the end of the early report interval.
The repository contains no test that repeats this measurement.

## 14. Limits

- Transition notifications are off by default. Without the option `WithNotifications`, an instance observes a transition at its next synchronization, at most one sync interval later.
- With notifications, each breaker of each instance holds one more connection to Redis.
- The adapter has no metrics of its own. `faultvaneotel.InstrumentStore` measures the duration of each operation. See [observability](../circuit-breaker/observability.md).
