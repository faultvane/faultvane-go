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

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names, for example `production`. |
| `WithKeyPrefix(prefix)` | `faultvane` | The first part of each key. |

## 2. Keys

Each breaker uses three keys:

| Key | Type | Content |
|---|---|---|
| `<prefix>:{<namespace>:<name>}:cb:state` | Hash | The authoritative state |
| `<prefix>:{<namespace>:<name>}:cb:window` | Hash | The shared window |
| `<prefix>:{<namespace>:<name>}:cb:probes` | Sorted set | The probe leases |

Example with the default prefix:

```text
faultvane:{production:payment-service}:cb:state
faultvane:{production:payment-service}:cb:window
faultvane:{production:payment-service}:cb:probes
```

Rules:

- The default prefix is `faultvane`. It is configurable.
- The namespace separates environments and groups of services. Instances that use the same namespace and name share one breaker.
- The part in braces is a Redis Cluster hash tag. The three keys of one breaker are always in the same slot. A script can then use all three keys.
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

## 8. How the adapter satisfies the contract

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
| C10 Cleanup | The TTL of the keys. See section 9. |
| C11 Data loss is visible | A key that does not exist gives `CLOSED` with generation 0. |
| C12 No side effects | The adapter has no goroutine, no lock, and no timer. |
| C13 Fingerprint | The field `fp` of the state hash. A script that accepts an event writes the fingerprint of its argument. Each reply contains the value from before the script. |

## 9. TTL

A script that accepts an event sets the same TTL on the three keys.
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
| An instance still holds a snapshot of an expired state | The instance handles this as data loss. See section 10. |

## 10. Data loss

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

## 11. Redis deployment

| Topic | Rule |
|---|---|
| Version | Redis 6.2 or later, or a compatible server. The tests of the project run with Redis 7 only. |
| Topology | Standalone and Cluster. The hash tag makes all scripts single-slot. The tests run against one standalone server and against one server with cluster mode on. The project has no test with Sentinel and no test with a cluster of many nodes. |
| Eviction | Use a policy that does not evict these keys, or accept that eviction is handled as data loss. |
| Persistence | Not necessary. Section 10 covers the loss of data. |
| Security | TLS and authentication are settings of the Redis client that the application supplies. |

## 12. Measured latency

| Operation | Time for each operation |
|---|---|
| `Report` | 0.13 ms |

The measurement used one client and sequential operations, with Redis in a local container.
[Benchmarks](../benchmarks.md) gives the machine and the method.
A network between the application and Redis adds its round-trip time.

## 13. Limits

- Transition notifications are not implemented. The adapter does not use Pub/Sub. An instance observes a transition at its next synchronization, at most one sync interval later.
- The adapter has no metrics of its own. `faultvaneotel.InstrumentStore` measures the duration of each operation. See [observability](../circuit-breaker/observability.md).
