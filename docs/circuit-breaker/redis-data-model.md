# Redis data model

This document defines the keys, the fields, the atomic scripts, and the TTL rules.
After version 1.0, this data model is a compatibility commitment.

## 1. Keys

Each breaker uses three keys:

| Key | Type | Content |
|---|---|---|
| `{prefix}:{<namespace>:<name>}:cb:state` | Hash | The authoritative state |
| `{prefix}:{<namespace>:<name>}:cb:window` | Hash | The shared window |
| `{prefix}:{<namespace>:<name>}:cb:probes` | Sorted set | The probe leases |

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
- The namespace and the name accept only the characters `a-z`, `A-Z`, `0-9`, `.`, `_`, and `-`. The maximum length of each is 64. This rule prevents collisions, because the separator `:` and the braces cannot occur in a name.
- `cb` identifies the circuit breaker. Later components use other identifiers under the same hash tag rules.

## 2. State hash

| Field | Type | Meaning |
|---|---|---|
| `v` | Integer | Schema version. The first version is `1`. |
| `st` | Integer | State: `0` `CLOSED`, `1` `OPEN`, `2` `HALF_OPEN`. |
| `gen` | Integer | Generation. |
| `ts` | Integer | Redis time of the last transition, in milliseconds. |
| `od` | Integer | Open duration of the current `OPEN` state, in milliseconds. |
| `ps` | Integer | Number of probe successes in the current `HALF_OPEN` state. |
| `cfg` | String | Fingerprint of the configuration of the last writer. |

A key that does not exist means `CLOSED` with generation 0.

## 3. Window hash

For each bucket, the hash has at most three fields:

| Field | Meaning |
|---|---|
| `<bucket>:c` | Calls |
| `<bucket>:f` | Failures |
| `<bucket>:s` | Slow calls |

`<bucket>` is the Redis time in milliseconds divided by the bucket duration, as an integer.
The report script deletes the fields of buckets that are older than the window size.
The hash has at most three fields for each bucket of the window, plus the fields of one more bucket.

## 4. Probes sorted set

| Part | Meaning |
|---|---|
| Member | The lease token. It is a random 128-bit value that the instance creates. |
| Score | The Redis time at which the lease ends, in milliseconds. |

A lease is valid when its score is larger than the current Redis time.

## 5. Scripts

Each change of the shared state runs in one Lua script.
Redis runs a script atomically.
Each script reads the time with `TIME` and returns the authoritative state after its work:

```text
(state, generation, transitionTime, openDuration, redisTime)
```

| Script | Arguments | Behavior |
|---|---|---|
| `report` | generation, calls, failures, slow calls, thresholds, window settings, open duration, fingerprint | Adds the counts to the shared window when the generation is equal and the state is `CLOSED`. Evaluates the failure condition. Makes transition T1 when the condition is true. |
| `acquire_probe` | token, probe limit, lease duration | Makes transition T2 when the state is `OPEN` and the open duration ended. In `HALF_OPEN`, removes the leases that ended, then adds the token when the number of valid leases is below the limit. Returns if the lease is granted. |
| `complete_probe` | generation, token, result, required successes, open duration | Does nothing when the generation is different or the token has no valid lease. Removes the lease. On failure, makes transition T4. On success, increments `ps` and makes transition T3 when `ps` reaches the required number. |
| `restore` | snapshot of an authoritative state | Writes the snapshot only when the key does not exist or the stored generation is lower than the snapshot generation. Deletes the window and the leases when it writes. |

A snapshot refresh without counts is a plain read of the state hash together with `TIME`.
It needs no script.

The [state machine](state-machine.md) defines the transitions T1 to T4.

### Script loading

The store calls scripts with `EVALSHA`.
When Redis answers `NOSCRIPT`, the store sends the script with `EVAL`.
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

## 6. TTL

Each script that writes sets the same TTL on the three keys.

```text
ttl = max(StateTTL, openDuration + windowSize + margin)
```

| Situation | Behavior |
|---|---|
| A breaker with traffic | Each report sets the TTL again. The state never expires. |
| `OPEN` state | The TTL is always longer than the remaining open duration. The state cannot expire before the open duration ends. |
| `HALF_OPEN` state | Each probe script sets the TTL again. Each lease also has its own end time. |
| A breaker with no traffic | The keys expire after the TTL. This is the cleanup. |
| An instance still holds a snapshot of an expired state | The instance handles this as data loss. See section 7. |

The default `StateTTL` is 1 hour.
The validation rejects a `StateTTL` that is shorter than two times the sum of the open duration and the window size.

## 7. Data loss

Redis can lose the state through eviction, a restart without persistence, a failover to a replica that is behind, or an expired TTL.

An instance detects data loss when the stored generation is lower than the generation of its snapshot, or when the key does not exist and its snapshot generation is above 0.

The instance then calls `restore` with its snapshot.
Only a snapshot that came from Redis is permitted. A state from local mode is never permitted.
If many instances call `restore`, the highest generation wins.
All instances then converge to that state.

Known limit: after a failover that loses a transition, a new transition can get a generation number that existed before with other content.
Each response contains the full state, and an instance always replaces its snapshot with the response.
The effect is limited to results that are in progress at that time. Probe tokens of the lost state are not valid in the new state.

## 8. Redis deployment

| Topic | Rule |
|---|---|
| Version | Redis 6.2 or later, or a compatible server. |
| Topology | Standalone, Sentinel, and Cluster. The hash tag makes all scripts single-slot. |
| Eviction | Use a policy that does not evict these keys, or accept that eviction is handled as data loss. |
| Persistence | Not necessary. Section 7 covers the loss of data. |
| Security | TLS and authentication are settings of the Redis client that the application supplies. |

## 9. Optional transition notifications

A script that makes a transition can publish the new generation on a Pub/Sub channel.
Instances that listen then refresh their snapshot immediately.
This function only makes the propagation faster.
The sync interval gives the guaranteed propagation.
The roadmap puts this function after the first release.
