# MongoDB adapter

This document describes the MongoDB adapter.
The [store contract](README.md) defines the rules for all adapters.
This document defines how the MongoDB adapter satisfies them: the document, the compare-and-set, the clock, and the cleanup.
After version 1.0, this data model is a compatibility commitment.

| Item | Value |
|---|---|
| Module | `github.com/faultvane/faultvane-go/store/mongodb` |
| Package name | `mongostore` |
| Minimum Go version | 1.25 |
| Driver | `go.mongodb.org/mongo-driver/v2` |
| Minimum MongoDB version | 4.4. `Load` uses an expression in a projection. |

## 1. Setup

```go
client, err := mongo.Connect(options.Client().ApplyURI(uri))
if err != nil {
    return err
}
collection := client.Database("resilience").Collection("circuit_breakers")

store, err := mongostore.New(collection, mongostore.WithNamespace("production"))
if err != nil {
    return err
}
if err := store.EnsureIndexes(ctx); err != nil {
    return err
}

breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(store))
if err != nil {
    return err
}
defer breaker.Close(ctx)
```

`New` returns an error that matches `faultvane.ErrInvalidConfig` in these conditions:

- The collection is `nil`.
- The namespace is not set, or it is not valid.

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names. It has 1 to 64 characters from `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`. |

| Method | Purpose |
|---|---|
| `EnsureIndexes(ctx)` | Creates the TTL index `faultvane_expire` on the field `expireAt`. Without this index, MongoDB does not remove old documents. |

The application selects the database and the collection.

## 2. Document

The adapter keeps one document for each breaker.

| Field | Type | Content |
|---|---|---|
| `_id` | String | `<namespace>:<name>` |
| `version` | Integer | A number that increases by 1 with each write of the record. The compare-and-set uses it. |
| `record` | String | The `circuitbreaker.Record` of the breaker as JSON text. |
| `expireAt` | Date | The time after which MongoDB can remove the document. |
| `now` | Date | The MongoDB server time of the last read step of an operation. The adapter uses it as the store time. |

The record contains the state, the generation, the transition time, the open duration, the number of probe successes, the leases, the window buckets, and the configuration fingerprint of the last writer.
The JSON field names are the names of the Go fields, for example `State`, `Generation`, `Leases`, `Buckets`, and `Fingerprint`.
A record without the field `Fingerprint` is valid. Its fingerprint is the empty text. A document from before the fingerprint needs no migration.
A new document has the record `{}` and the version 0. That is `CLOSED` with generation 0.
The namespace and the name cannot contain the separator `:`, so two breakers cannot get the same `_id`.

## 3. Operations

`Report`, `AcquireProbe`, `CompleteProbe`, and `Restore` each use an optimistic compare-and-set:

1. **Read step.** `findOneAndUpdate` with an update pipeline and `upsert`. It writes the server time `$$NOW` to the field `now`. It creates the document when the document does not exist. It returns the document.
2. Apply the event to the record with the method of `circuitbreaker.Record`, through `Record.Apply`. This step runs in the instance.
3. **Write step.** `updateOne` with a filter on `_id` and on the `version` of the read step. It writes the record, the version plus 1, and the new `expireAt`.
4. When the write step matches no document, a different instance changed the record in the meantime. The adapter starts again at step 1.

The adapter makes at most 16 attempts for one operation.
After 16 attempts, the operation returns a store fault.
A duplicate key error of two concurrent first operations on a new breaker also uses one attempt.

`Load` is one `findOne` with a projection that adds `$$NOW`. It writes nothing.
When the document does not exist, `Load` reads the server time with the `hello` command and returns `CLOSED` with generation 0.

Each write operation writes the document and sets `expireAt`, also when the record ignores the event.
The adapter uses no multi-document transaction. It does not need a replica set for transactions.

## 4. How the adapter satisfies the contract

| Rule | How |
|---|---|
| C1 Atomic | The write step is one update of one document. MongoDB applies it fully or not at all. |
| C2 Ordered | The filter on `version`. A write step succeeds only when no other write occurred after its read step. |
| C3 State machine | The methods of `circuitbreaker.Record`. The adapter contains no transition logic. |
| C4 Generation | The methods of `circuitbreaker.Record`. |
| C5 One clock | The MongoDB variable `$$NOW`. See section 5. |
| C6 Leases | The record holds each lease with its end time in MongoDB time. |
| C7 Isolation | The `_id` contains the namespace and the name. |
| C8 Context | Each command uses the context of the operation. The number of attempts has a limit of 16. |
| C9 Errors | Each error of the driver, each record that is not valid JSON, and the end of the 16 attempts match `faultvane.ErrStoreUnavailable`. The message starts with `mongostore:` and the name of the operation. |
| C10 Cleanup | The field `expireAt` and a TTL index. See section 6. |
| C11 Data loss is visible | A document that does not exist gives `CLOSED` with generation 0. |
| C12 No side effects | The adapter has no goroutine, no lock, and no timer. |
| C13 Fingerprint | The field `Fingerprint` of the record. The method `Record.Apply` gives the response with the fingerprint from before the operation. An attempt that loses the compare-and-set gives no response. |

The adapter passes the conformance suite with MongoDB.

## 5. Clock

The clock is the MongoDB variable `$$NOW`, the time of the MongoDB server.
Its precision is 1 millisecond.
The clocks of the instances have no effect on the shared state.

A bucket duration or a lease duration below 1 millisecond is not exact with this adapter.

## 6. Cleanup

| Operation | Value of `expireAt` |
|---|---|
| `Report`, `AcquireProbe`, `CompleteProbe` | MongoDB time plus the effective state TTL of the breaker |
| `Restore` | MongoDB time plus 1 hour |

[Configuration](../circuit-breaker/configuration.md) defines the effective state TTL.

`EnsureIndexes` creates a TTL index on `expireAt` with `expireAfterSeconds` 0.
The TTL monitor of MongoDB removes a document after its `expireAt` time.
The TTL monitor runs in the background at an interval. A document can stay for some time after its `expireAt` time.
The adapter has no cleanup of its own.

Call `EnsureIndexes` one time when the application starts, or create the same index with your deployment tools.
Do this before the application gets traffic. The call also creates the collection. Without it, the first write creates the collection, and that write can be slower than the store timeout. The breaker does not send a failed report again.

## 7. Measured latency

| Operation | Time for each operation |
|---|---|
| `Load` | 0.17 ms |
| `Report` | 0.50 ms |

The measurement used one client and sequential operations, with MongoDB in a local container.
[Benchmarks](../benchmarks.md) gives the machine and the method.
A network between the application and MongoDB adds its round-trip time for each command.

## 8. Limits

- A write operation without contention uses two commands. Each lost race adds two commands.
- Many instances that write one breaker at the same time can use all 16 attempts. The operation then returns a store fault, and the breaker counts it as one store fault.
- The request path does not call the store in the `CLOSED` state, so the latency of MongoDB does not change the latency of a normal call. It changes the lowest practical sync interval. The project did not measure this interval.
- `Load` is a read. The adapter uses the read settings of the collection that the application supplies. With a read preference that permits a secondary, `Load` can return an old state. The project has no test for this condition.
- The tests run with MongoDB 7 as one server. The project has no test with MongoDB 4.4 and no test with a replica set or a sharded cluster.
- The data loss handling is the same as for each adapter. When a document is lost, an instance calls `Restore` with its last authoritative snapshot. [Degraded mode](../circuit-breaker/degraded-mode.md) describes it.
- The adapter has no metrics of its own. `faultvaneotel.InstrumentStore` measures the duration of each operation.

## 9. Acceptance tests

The file `store/mongodb/acceptance_test.go` runs these acceptance tests of the [requirements](../circuit-breaker/requirements.md) against MongoDB.

| Test | Function | Method |
|---|---|---|
| AT-3 | `TestAT3_RequestsContinueWithoutTheStore` | The test stops the connection of one instance. The instance changes to local mode. All calls succeed. The local breaker opens after 2 failures. |
| AT-4 | `TestAT4_ReconciliationKeepsTheNewerState` | The test stops the connection of one instance. A second instance opens the circuit in MongoDB. The test starts the connection again. The first instance takes the state `OPEN` with generation 1. |
| AT-5 | `TestAT5_StaleWriterIsRejected` | MongoDB has `HALF_OPEN` with generation 100. Two instances get a probe. The first instance does the read step for its probe success. Then the probe failure of the second instance gives `OPEN` with generation 101. The write step of the first instance matches no document. Its second attempt changes nothing. MongoDB keeps `OPEN` with generation 101. |
| AT-6 | `TestAT6_InstancesConvergeAfterTheOutage` | The test stops the connections of four instances. Two instances open locally and two stay closed. A report through a direct connection opens the circuit in MongoDB. The test starts the connections again. All instances get `OPEN` with generation 1 in distributed mode. |

Each instance has its own MongoDB client.
The test gives the client its own dialer.
To stop a connection, the test closes all TCP connections of the client and refuses each new connection.
The test does not stop the MongoDB container.

The tests need the MongoDB of `docker-compose.test.yml`.
`sh scripts/test-stores.sh` starts it.

## 10. Rate limit store

The module also contains a store for the shared limit of the rate limiter: `mongostore.NewRateLimitStore`.
It uses the option, the clock, the compare-and-set, and the TTL index of this adapter.
It keeps one document for each name with the identifier `<namespace>:<name>:rl:bucket`. The document can be in the collection of the circuit breakers.
The [rate limit store](../ratelimit/store.md) describes the document, the operation, and the limits.
