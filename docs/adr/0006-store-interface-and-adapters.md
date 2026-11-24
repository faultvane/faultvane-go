# ADR 0006: Store interface with adapters

Status: Accepted

## Context

The first design named Redis as the shared store.
The owner of the project decided that the shared state must not depend on one product.
An application must be able to use Redis, a SQL database, or another system with shared state.

The breaker needs atomic operations that read and write many values.
Different stores give atomic behavior in different ways: a script, a transaction, or a compare-and-set.

## Decision

1. The core defines one `Store` interface. It has one operation for each event of the state machine, and one read operation: `Load`, `Report`, `AcquireProbe`, `CompleteProbe`, and `Restore`.
2. An adapter implements the interface for one type of store. The adapter selects the atomic mechanism.
3. The core owns all behavior that is not storage: the synchronizer, timeouts, fault detection, local mode, reconciliation, and observability.
4. A written contract defines the rules for an adapter. One conformance test suite checks them. An adapter is correct when it passes the suite.
5. The in-memory adapter is in the core module. Each adapter with an external dependency is a separate module.
6. The first release contains the in-memory adapter and the Redis adapter. The SQL adapter follows.

ADR 0002 and ADR 0003 stay valid with a smaller scope.
ADR 0002 now describes how the Redis adapter gives atomic operations.
ADR 0003 now describes the clock of the Redis adapter. The general rule is that each adapter uses one clock for all instances.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| A low interface with `Get`, `Set`, `SetIfAbsent`, and `Publish` | The core must then build atomic transitions from separate calls. That is not possible without a compare-and-set on many values, and each store gives different guarantees. The result is last-write-wins behavior, which the generation rules forbid. |
| A compare-and-set interface on one opaque record, with the state machine only in Go | It fits all stores, and the logic is in one place. But each report then needs a read and a conditional write, and a failed condition needs another round. Contention is highest during an outage, when the breaker must be fast. Redis can do the full operation in one round trip. An adapter is free to use this approach internally. |
| Redis only | It does not satisfy the decision of the owner. |
| One adapter for each store with its own copy of the degraded mode logic | The behavior would differ between stores. The core keeps this logic one time. |

## Consequences

Positive:

- The application selects the store. An application without Redis can use its database.
- The core has no dependency on a store client.
- The conformance suite gives a clear and testable definition of a correct adapter.
- The in-memory adapter makes fast tests of the core possible without a container.

Negative:

- An adapter that uses a script or a query language contains a second implementation of the transitions. The conformance suite must keep it equal to the state machine.
- The interface limits the stores to those with an atomic conditional update and one clock.
- Functions of one store, for example Redis Pub/Sub for faster propagation, are optional additions to an adapter. The core cannot depend on them.
- Each new adapter adds test infrastructure to continuous integration.

## Examine again when

- A store that many users need cannot implement the interface.
- The second implementations of the transitions in the adapters cause defects that the conformance suite does not find.
