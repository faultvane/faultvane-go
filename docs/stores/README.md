# Stores

The breaker keeps its shared state in a **store**.
The breaker does not know the type of the store.
It uses the store through one interface.
An **adapter** implements the interface for one type of store.

This document is about the stores of the circuit breaker. The rate limiter has a different store interface. See the [rate limit store](../ratelimit/store.md).

## 1. Adapters

| Adapter | Go package | Module | Status |
|---|---|---|---|
| In-memory | `circuitbreaker` (`NewMemoryStore`) | Core | Implemented |
| [Redis](redis.md) | `redisstore` in `store/redis` | Separate | Implemented |
| [SQL database](sql.md) | `sqlstore` in `store/sql` | Separate | Implemented for PostgreSQL and MySQL |
| [MongoDB](mongodb.md) | `mongostore` in `store/mongodb` | Separate | Implemented |
| Your own | Your package | Your module | See section 6 |

No module has a release tag yet.

The in-memory adapter keeps the state in one process.
Use it for tests and for an application with one instance.
It is also the reference implementation of the contract.

```go
store := circuitbreaker.NewMemoryStore(faultvane.SystemClock())
```

`NewMemoryStore` gets the clock of the store as its argument.
The method `Delete` removes the data of one breaker. Tests use it to simulate data loss.

## 2. Interface

The interface has one operation for each event of the [state machine](../circuit-breaker/state-machine.md), and one read operation.
It does not expose keys, queries, or scripts.

```go
type Store interface {
    Load(ctx context.Context, name string) (Snapshot, error)
    Report(ctx context.Context, name string, report Report) (Snapshot, error)
    AcquireProbe(ctx context.Context, name string, request ProbeRequest) (ProbeGrant, error)
    CompleteProbe(ctx context.Context, name string, result ProbeResult) (Snapshot, error)
    Restore(ctx context.Context, name string, snapshot Snapshot) (Snapshot, error)
}
```

| Operation | Purpose | State change |
|---|---|---|
| `Load` | Read the authoritative state. | None |
| `Report` | Add counts to the shared window and evaluate the failure condition. | T1 |
| `AcquireProbe` | Request a probe lease. | T2 |
| `CompleteProbe` | Send the result of a probe. | T3 or T4 |
| `Restore` | Write an authoritative snapshot again after data loss. | Only when the generation is higher |

Each operation returns the authoritative state after its work, as a `Snapshot`:

```text
Snapshot(state, generation, transitionTime, openDuration, storeTime, fingerprint)
```

The fingerprint of a snapshot is the configuration fingerprint that the store had before the operation. Rule C13 defines it.

The values that an operation needs, for example the thresholds, the window settings, and the state TTL, are fields of its argument.
The store keeps no configuration.

### Optional interface: transition notifications

An adapter can also implement the interface `Notifier`:

```go
type Notifier interface {
    Subscribe(ctx context.Context, name string, changed func()) (stop func(), err error)
}
```

The breaker examines its store when it starts. When the store implements `Notifier`, the breaker subscribes.
The adapter calls `changed` after a transition of the breaker with that name. The breaker then synchronizes and does not wait for the sync interval.

Rules for an adapter:

- `Subscribe` obeys its context and returns quickly. The context applies only to the start of the subscription.
- `changed` can run in each goroutine. It does not block.
- After `stop` returns, the adapter does not call `changed` again and holds no goroutine and no connection for the subscription.
- A notification carries no state. The contract rules C1 to C14 do not depend on it. An adapter that loses, delays, or repeats a notification is still correct.
- A wrapper around a store must pass `Subscribe` to the store. If it does not, the breaker gets no notifications and uses the sync interval only.

The interface is not a part of `Store`. An adapter without it is complete.
Only the Redis adapter implements it, as an option. The in-memory, SQL, and MongoDB adapters do not.
[Architecture](../circuit-breaker/architecture.md) describes the behavior of the breaker. [ADR 0009](../adr/0009-transition-notifications.md) records the decision.

### The record type

The package `circuitbreaker` exports the type `Record`.
A `Record` is the full shared data of one breaker: the state, the generation, the transition time, the open duration, the number of probe successes, the leases, the window buckets, and the configuration fingerprint of the last writer.
It is a pure value. Its methods make no I/O and read no clock.

| Method | Store operation |
|---|---|
| `Report(now, report) Record` | `Report` |
| `AcquireProbe(now, request) (Record, bool)` | `AcquireProbe` |
| `CompleteProbe(now, result) Record` | `CompleteProbe` |
| `Restore(snapshot) Record` | `Restore` |
| `Snapshot(now) Snapshot` | The response of `Load` |
| `Apply(now, change) (Record, Snapshot)` | The new record and the response of each operation that writes. The response has the fingerprint of the record before the change. |

An adapter written in Go can use `Record` for all transition logic.
The adapter then does only three things: read the record and the store time atomically, call the method through `Apply`, and write the result.
The in-memory, SQL, and MongoDB adapters work in this way. They share one implementation of the transitions.
The Redis adapter cannot run Go code in the store. Its Lua scripts are a second implementation.
[ADR 0007](../adr/0007-shared-record-for-go-adapters.md) records this decision.

### Why the interface is at this level

A lower interface with `Get`, `Set`, and `SetIfAbsent` looks more general.
But the breaker needs operations that read and write many values atomically.
With a low interface, the atomic behavior depends on how the core combines the calls, and each type of store gives different guarantees for that.
With one operation for each event, each adapter uses the best atomic mechanism of its store.
[ADR 0006](../adr/0006-store-interface-and-adapters.md) records this decision.

## 3. Contract

An adapter must satisfy these rules.

| ID | Rule |
|---|---|
| C1 | **Atomic.** Each operation applies all its changes or none. No other operation on the same breaker sees a partial result. |
| C2 | **Ordered.** The operations on one breaker have one total order. An operation sees the result of all operations that completed before it started. |
| C3 | **State machine.** An operation changes the state only as the [state machine](../circuit-breaker/state-machine.md) defines. It ignores an event that is not valid and still returns the current state. |
| C4 | **Generation.** Each transition increases the generation by exactly 1. An event with another generation changes nothing. |
| C5 | **One clock.** All times of the shared state come from one clock that is the same for all instances. Each response contains the current time of this clock. |
| C6 | **Leases.** The number of valid leases never exceeds the probe limit. A lease ends at its end time without an action of its owner. |
| C7 | **Isolation of breakers.** Two breakers with different namespaces or names never share data. |
| C8 | **Context.** Each operation stops when its context ends and returns the context error. It does not retry internally without a limit. |
| C9 | **Errors.** A fault of the store returns an error that matches `faultvane.ErrStoreUnavailable`. An event that the store ignores is not an error. |
| C10 | **Cleanup.** The data of a breaker that is not used does not stay forever. Cleanup never removes the state of a breaker in use. |
| C11 | **Data loss is visible.** After a loss of data, `Load` returns a lower generation or the initial state. The adapter does not hide the loss. |
| C12 | **No side effects on the request path.** The adapter starts no goroutine for each call and holds no lock across a network call. |
| C13 | **Fingerprint.** The data of a breaker contains one configuration fingerprint. `Report`, `AcquireProbe`, and `CompleteProbe` save the fingerprint of their argument when the store accepts the operation. An operation that the store ignores and `Restore` do not change the fingerprint. A transition keeps it. Each response contains the fingerprint that the store had before the operation. Data without a fingerprint gives the empty text. |
| C14 | **Report identity.** A report with a sequence above 0 has an identity: its instance number and its sequence. For each instance number, the store keeps the highest sequence of the reports that it accepted. The store ignores a report whose sequence is equal to or lower than this value. The instance numbers are independent of each other. The store adds a report with the sequence 0 each time. The store can forget an instance number when the counts of its last report left the shared window. A transition and `Restore` can remove all instance numbers. |

For rule C13, the store accepts an operation in these conditions:

| Operation | Condition |
|---|---|
| `Report` | The state is `CLOSED`, the generation of the report is equal to the stored generation, and rule C14 does not ignore the report. |
| `AcquireProbe` | The state after the operation is `HALF_OPEN`. The store saves the fingerprint also when it grants no lease. |
| `CompleteProbe` | The state is `HALF_OPEN` and the generation of the result is equal to the stored generation. The store saves the fingerprint also when the token has no valid lease. |

The fingerprint is text without a meaning for the store. The store does not compare fingerprints.
[Failure detection](../circuit-breaker/failure-detection.md) describes how the breaker uses the fingerprint.

A breaker with no stored state is `CLOSED` with generation 0.

### What a store must be able to do

An adapter is possible for a store that has these two properties:

1. **An atomic conditional update of one record or one group of records.** Examples are a script, a transaction with a row lock, or a compare-and-set.
2. **One clock for all instances.** The clock of the store server is the usual source.

A store without the first property cannot satisfy C1 and C2. Do not write an adapter for it.

## 4. Division of work

| Part | Owner |
|---|---|
| Permit decision, local window, pending counts | Core |
| Synchronizer, timeouts, fault detection, local mode, reconciliation | Core |
| Logs, events, counters | Core |
| OpenTelemetry metrics and trace attributes | The module `otel` |
| The five operations and their atomic behavior | Adapter |
| The optional transition notifications: the channel, the message, and the connection | Adapter |
| The reaction to a notification and its rate limit | Core |
| The data layout in the store and its cleanup | Adapter |
| The connection, TLS, and authentication | The client that the application supplies |

Degraded mode, reconciliation, and observability work the same with each adapter, because the core owns them.
An adapter is small. It contains no timers and no goroutines. The exception is the optional subscription of the Redis adapter: it has one goroutine for each subscription.
An adapter does not retry a failed operation. The MongoDB adapter repeats its compare-and-set at most 16 times when a different instance wrote first.

## 5. Conformance tests

The package `circuitbreaker/storetest` contains one test suite for the contract.
Each adapter runs the suite against a real instance of its store.

```go
func TestConformance(t *testing.T) {
    client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"})
    t.Cleanup(func() { _ = client.Close() })

    storetest.Run(t, func(t *testing.T) circuitbreaker.Store {
        namespace := fmt.Sprintf("test-%d", time.Now().UnixNano())
        store, err := redisstore.New(client, redisstore.WithNamespace(namespace))
        if err != nil {
            t.Fatal(err)
        }
        return store
    })
}
```

`Run` calls the function one time for each check. Each check uses its own breaker name.

The suite contains:

- Checks for the transitions, the generation rules, the window, the leases, restore, the fingerprint, the isolation of breakers, the store time, and an ended context. The name of each check starts with the contract rule that it proves, for example `C6`.
- Concurrency checks for C1, C4, and C6: concurrent probe requests get exactly the limit, and concurrent reports open the circuit one time.
- Random sequences of operations. The suite runs each sequence on the adapter and on the in-memory store. The states, the generations, the probe grants, and the fingerprints of the responses must be equal.

The suite cannot control the clock of a real store. It uses short durations and waits for a condition with a time limit.

An adapter is correct when it passes the suite.

### Acceptance tests with each adapter

The acceptance tests of the [requirements](../circuit-breaker/requirements.md) run in these places:

| Test | In-memory | Redis | SQL | MongoDB |
|---|---|---|---|---|
| AT-1 Shared failure detection | Yes | Yes | Yes | Yes |
| AT-2 One probe for all instances | Yes | Yes | Yes | Yes |
| AT-3 Operation without the store | Yes | Yes | Yes | Yes |
| AT-4 Reconciliation | Yes | Yes | Yes | Yes |
| AT-5 Stale write | Yes | No | Yes | Yes |
| AT-6 No split-brain condition | Yes | Yes | Yes | Yes |
| AT-7 System benchmark | No | Yes | No | No |
| Restore after data loss (RF-5) | Yes | Yes | Yes | Yes |

The core owns local mode and reconciliation, so AT-3, AT-4, and AT-6 do not depend on the adapter. The tests with the SQL adapter and the MongoDB adapter stop the connections of an instance. They do not stop the store.
For AT-5, the conformance suite has a check with each adapter: a report with a different generation adds nothing. The Redis adapter has no separate AT-5 test.

## 6. Write your own adapter

1. Implement the `Store` interface in your own module. When the adapter is in Go and the store has no script language, use `circuitbreaker.Record` for the transitions.
2. Decide how your store gives rule C1. Write it in the documentation of the adapter.
3. Decide which clock gives rule C5.
4. Run `storetest.Run` against a real instance of your store. Do not use a mock.
5. Document the limits of your store: latency, data loss conditions, and cleanup.

## 7. Adapter documents

| Adapter | Document | Atomic mechanism |
|---|---|---|
| Redis | [Redis adapter](redis.md) | One Lua script for each operation |
| PostgreSQL, MySQL | [SQL adapter](sql.md) | One transaction with `SELECT ... FOR UPDATE` on the row of the breaker |
| MongoDB | [MongoDB adapter](mongodb.md) | Compare-and-set on a version field of one document |

Each document gives the data layout, the clock, the cleanup, the setup code, the limits, and the measured latency.

A database has more latency for each operation than Redis, and each report is a write.
This does not affect application requests, because the request path does not call the store in the `CLOSED` state.
It affects the lowest practical sync interval.

## 8. Candidate adapters

This table shows which stores can satisfy the contract.

| Store | Fits the contract | Atomic mechanism | Clock | Status |
|---|---|---|---|---|
| Redis | Yes | Lua script | `TIME` | Implemented |
| PostgreSQL, MySQL | Yes | Transaction with a row lock | Database server time | Implemented |
| MongoDB | Yes | Compare-and-set on a version field of one document for each breaker | `$$NOW`. Needs MongoDB 4.4 or later. | Implemented |
| etcd | Yes | Transaction with a compare on the revision | Leases of the server | Candidate |
| DynamoDB | Partly | Conditional write | No server clock that a write can read. The adapter needs a different clock source. | Candidate |
| Memcached | No | Compare-and-set on one value only | None | Not possible |

A candidate becomes a milestone of the [roadmap](../roadmap.md) when a user needs it.
The statements for the rows with the status `Candidate` or `Not possible` come from the documentation of the stores. The project did not test them.

Each adapter has a cost: tests against a real store with the full coverage minimum, the conformance suite, a container in continuous integration, and maintenance for each new version of the store and of its client library.
After version 1.0, the data layout of an adapter is a compatibility commitment.
For this reason, the project adds an adapter only when it has users.

## 9. Comparison of the adapters

| Property | In-memory | Redis | SQL database | MongoDB |
|---|---|---|---|---|
| Shares state between instances | No | Yes | Yes | Yes |
| Atomic mechanism | Mutex | Lua script | Transaction with row lock | Compare-and-set on a version |
| Implementation of the transitions | `Record` | Lua scripts | `Record` | `Record` |
| Clock | The clock that the application gives | Redis server time, microseconds | Database server time, microseconds | MongoDB server time, milliseconds |
| Cleanup | None. `Delete` removes one breaker. | Key TTL | Delete of expired rows, at most one time each minute after a report | TTL index |
| Setup step | None | None | `Migrate` | `EnsureIndexes` |
| Transition notifications | No | Optional, with Pub/Sub | No | No |
| Measured `Load` | Not measured | Not measured | 0.29 ms (PostgreSQL), 0.26 ms (MySQL) | 0.17 ms |
| Measured `Report` | Not measured | 0.13 ms | 0.86 ms (PostgreSQL), 2.0 ms (MySQL) | 0.50 ms |
| Extra infrastructure | None | Redis | The database of the application | MongoDB |

The measured values come from one machine with each store in a local container. [Benchmarks](../benchmarks.md) gives the conditions.
