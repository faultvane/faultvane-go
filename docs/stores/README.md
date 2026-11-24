# Stores

The breaker keeps its shared state in a **store**.
The breaker does not know the type of the store.
It uses the store through one interface.
An **adapter** implements the interface for one type of store.

## 1. Adapters

| Adapter | Package | Module | Status |
|---|---|---|---|
| In-memory | `store/memory` | Core | Planned for the first release |
| [Redis](redis.md) | `store/redis` | Separate | Planned for the first release |
| SQL database | `store/sql` | Separate | Planned after the first release |
| Your own | Your package | Your module | See section 6 |

The in-memory adapter keeps the state in one process.
Use it for tests and for an application with one instance.
It is also the reference implementation of the contract.

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
Snapshot(state, generation, transitionTime, openDuration, storeTime)
```

The values that an operation needs, for example the thresholds and the window settings, are fields of its argument.
The store keeps no configuration.

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
| Logs, metrics, events | Core |
| The five operations and their atomic behavior | Adapter |
| The data layout in the store and its cleanup | Adapter |
| The connection, TLS, and authentication | The client that the application supplies |

Degraded mode, reconciliation, and observability work the same with each adapter, because the core owns them.
An adapter is small. It contains no timers, no retries, and no goroutines.

## 5. Conformance tests

The package `store/storetest` contains one test suite for the contract.
Each adapter runs the suite against a real instance of its store.

```go
func TestConformance(t *testing.T) {
    storetest.Run(t, func(t *testing.T) circuitbreaker.Store {
        return redisstore.New(newClient(t), redisstore.WithNamespace(storetest.Namespace(t)))
    })
}
```

The suite contains:

- A test for each branch of each operation.
- The same random event sequences that the property tests of the state machine use. The result of the adapter must be equal to the result of the pure state machine.
- Concurrency tests for C1, C2, C4, and C6.
- Tests for expired leases, with a clock that the suite controls where the store permits it.

An adapter is correct when it passes the suite.
The acceptance tests AT-1 to AT-6 of the [requirements](../requirements.md) also run against each adapter of the repository.

## 6. Write your own adapter

1. Implement the `Store` interface in your own module.
2. Decide how your store gives rule C1. Write it in the documentation of the adapter.
3. Decide which clock gives rule C5.
4. Run `storetest.Run` against a real instance of your store. Do not use a mock.
5. Document the limits of your store: latency, data loss conditions, and cleanup.

## 7. Notes on the planned SQL adapter

The detailed design comes with its milestone.
These points are fixed now, because they show that the interface fits a database:

| Topic | Approach |
|---|---|
| Atomic operation | One transaction for each operation, with a lock on the row of the breaker. |
| Data layout | One row for each breaker with the state, the generation, and the times. The window buckets and the leases are in the same row or in two child tables. |
| Clock | The time of the database server. |
| Cleanup | A delete of rows that had no write for longer than the state TTL. |
| API | `database/sql`, so the application selects the driver. |

A database has more latency for each operation than Redis, and each report is a write transaction.
This does not affect application requests, because the request path does not call the store.
It affects the lowest practical sync interval.
The SQL adapter will document measured limits.

## 8. Comparison of the adapters

| Property | In-memory | Redis | SQL database |
|---|---|---|---|
| Shares state between instances | No | Yes | Yes |
| Atomic mechanism | Mutex | Lua script | Transaction with row lock |
| Clock | Process clock | Redis server time | Database server time |
| Cleanup | Not necessary | Key TTL | Delete of old rows |
| Typical latency of an operation | Nanoseconds | Below 1 ms | Milliseconds |
| Extra infrastructure | None | Redis | The database of the application |
