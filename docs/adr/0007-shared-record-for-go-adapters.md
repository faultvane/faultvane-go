# ADR 0007: One exported record type for the adapters written in Go

Status: Accepted

## Context

ADR 0006 defines the `Store` interface and lets each adapter select its atomic mechanism.
It also names a risk: an adapter can contain a second implementation of the transitions.

The project now has four adapters: in-memory, Redis, SQL, and MongoDB.
Three of them run the transition logic in the Go process:

- The in-memory adapter holds the data in a map behind a mutex.
- The SQL adapter reads a row with a lock, changes it, and writes it in one transaction.
- The MongoDB adapter reads a document, changes it, and writes it with a compare-and-set on a version.

Each of these adapters needs the same logic: the shared window, the failure condition, the leases, the transitions, and the generation rules.
Three copies of this logic are three places for a defect.

The Redis adapter is different. A Redis script must do the full operation in the store, in Lua.

## Decision

1. The package `circuitbreaker` exports the pure type `Record`. A `Record` is the full shared data of one breaker: the state, the generation, the transition time, the open duration, the number of probe successes, the leases, and the window buckets.
2. `Record` has one method for each event of the store interface: `Report`, `AcquireProbe`, `CompleteProbe`, and `Restore`. The method `Snapshot` gives the response of an operation. The methods make no I/O and read no clock. The time is an argument.
3. The in-memory, SQL, and MongoDB adapters use `Record` for all transition logic. Each adapter does only the storage work: an atomic read of the record and of the store time, the call of the method, and the write of the result. The SQL and MongoDB adapters keep the record as JSON text.
4. The local breaker uses the same `Record` for its local state.
5. The Redis adapter keeps its Lua scripts. They are a second implementation of the transitions.
6. The conformance suite compares each adapter with the reference. It runs the same random sequences of operations on the adapter and on the in-memory store. The states, the generations, and the probe grants must be equal.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| Keep the record type internal and write the logic again in each adapter | Each adapter module then contains a copy of the transitions. A change of a rule needs a change in each copy. The adapter modules are separate modules, so they cannot import an internal package of the core. |
| Write the SQL transitions in SQL statements or stored procedures | The logic then exists one more time, in one more language, for each dialect. Go coverage cannot measure it. |
| Write the MongoDB transitions in an update pipeline | The same problem. The window and the lease rules are complex in a pipeline. |
| Change the Redis adapter to a compare-and-set on one opaque record | Redis would need two round trips for each operation and a retry on contention. The script does the operation in one round trip. Contention is highest during an outage, when the breaker must be fast. |

## Consequences

Positive:

- The transitions have one implementation in Go for three adapters and for the local breaker. Unit tests, property tests, and the fuzz target cover it without a store.
- The SQL and MongoDB adapters are small. They contain no transition logic.
- A person who writes an adapter in Go for a store without a script language can use `Record`.

Negative:

- `Record` and its field names are public API. The SQL and MongoDB adapters store the record as JSON with the Go field names. After version 1.0, a change of a field is a change of stored data, and it is a compatibility commitment.
- An adapter that uses `Record` moves the full record for each operation. It needs a read and a write. This costs more round trips than a script in the store.
- The Lua scripts stay a second implementation. Only the conformance suite keeps them equal to `Record`.
- Go coverage does not measure the Lua scripts.

## Examine again when

- The conformance suite does not find a difference between the Lua scripts and `Record` that causes a defect.
- The size of the record or the number of round trips limits the sync interval of an adapter.
- A change of the record format is necessary after version 1.0.
