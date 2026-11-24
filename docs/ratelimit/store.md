# Rate limit store

This document describes the store of the shared rate limit.
The [rate limiter guide](README.md) describes how a limiter uses the store.
The [requirements](requirements.md) RL-20 to RL-41 and RL-48 to RL-58 apply.

After version 1.0, the interface, its contract, the type `Bucket`, and the data layout of the Redis store, the SQL store, and the MongoDB store are compatibility commitments.

## 1. Stores

| Store | Package | Use |
|---|---|---|
| In-memory | `github.com/faultvane/faultvane-go/ratelimit` | Tests, and limiters in one process. It is the reference implementation. |
| Redis | `github.com/faultvane/faultvane-go/store/redis` (package `redisstore`) | Instances in many processes. |
| SQL | `github.com/faultvane/faultvane-go/store/sql` (package `sqlstore`) | Instances in many processes, with PostgreSQL or MySQL. |
| MongoDB | `github.com/faultvane/faultvane-go/store/mongodb` (package `mongostore`) | Instances in many processes. |

No other store exists.

## 2. Interface

```go
type Store interface {
    Acquire(ctx context.Context, name string, request Acquire) (Grant, error)
}

type Acquire struct {
    Wanted   int64
    Returned int64
    Rate     float64
    Burst    int64
    StateTTL time.Duration
}

type Grant struct {
    Tokens    int64
    StoreTime time.Time
}
```

| Field | Content |
|---|---|
| `Acquire.Wanted` | The number of tokens that the limiter asks for. |
| `Acquire.Returned` | The number of tokens that the limiter gives back. |
| `Acquire.Rate` | The number of tokens that the shared bucket gets each second. |
| `Acquire.Burst` | The largest number of tokens that the shared bucket can hold. |
| `Acquire.StateTTL` | The time after which the store can remove a bucket that no limiter uses. |
| `Grant.Tokens` | The number of tokens that the store gives to the limiter. |
| `Grant.StoreTime` | The time of the store clock in this operation. |

The store keeps one bucket for each name. The bucket has two values: the number of tokens and the time of the last operation.

One `Acquire` operation does these steps as one atomic change:

1. It reads the time of the store clock. When this time is before the time of the last operation, it uses the time of the last operation.
2. It adds `Rate` multiplied by the elapsed seconds to the tokens.
3. It adds `Returned` to the tokens.
4. It decreases the tokens to `Burst` when they are above `Burst`.
5. It grants the smaller of `Wanted` and the whole tokens of the bucket. It grants 0 when `Wanted` is below 0.
6. It removes the granted tokens from the bucket and stores the time.

The interface has one operation because the limiter needs one atomic step: give back, refill, and take.
The limiter sends the rate and the burst in each operation. The store keeps no configuration.

### Bucket

```go
type Bucket struct {
    Tokens float64
    At     time.Time
}

func (b Bucket) Acquire(now time.Time, request Acquire) (Bucket, Grant)
```

The type `Bucket` contains the calculation of the steps above.
`Tokens` is the number of tokens. `At` is the time of the last operation.
The method `Acquire` gets the time of the store clock as an argument. It returns the bucket after the operation and the grant.
It makes no I/O and reads no clock.

The in-memory store, the SQL store, and the MongoDB store use this method. Thus the calculation has one implementation for these three stores.
Each of these stores does only the storage work: it reads the bucket and the store time, calls the method, and writes the result as one atomic change.
A store makes the full bucket of a name without state: `Tokens` is `Burst`, and `At` is the store time.
The Redis store has the same calculation in its script, because a Redis script cannot call the method.

The limiter sets `StateTTL` to the time that an empty bucket needs to become full, plus one minute.
A bucket that the store removes after this time is equal to a full bucket. Thus the removal changes no result.

## 3. Contract

A store must satisfy these rules.

| ID | Rule |
|---|---|
| RS1 | **Atomic.** Each operation applies all its changes or none. Concurrent operations on one name never grant more tokens in total than the bucket holds. They also lose no token. |
| RS2 | **Full start.** A name without state has a full bucket. The bucket holds `Burst` tokens. |
| RS3 | **Grant.** The grant is never above `Wanted`, never above the whole tokens of the bucket, and never below 0. |
| RS4 | **One clock.** The bucket refills by the clock of the store, at `Rate` tokens each second, up to `Burst`. Each response contains the time of this clock. The time does not go back. |
| RS5 | **Returned tokens.** The store adds the returned tokens before it calculates the grant. The bucket never holds more than `Burst`. |
| RS6 | **Isolation.** Two names never share data. |
| RS7 | **Context.** An operation stops when its context ends. It returns an error and changes nothing. |
| RS8 | **Errors.** A fault of the store returns an error that matches `faultvane.ErrStoreUnavailable`. |
| RS9 | **Cleanup.** The data of a name that no limiter uses does not stay forever. The store keeps the data at least for the time `StateTTL` after the last operation. |

The in-memory store does not satisfy RS9. It keeps one small record for each name until the process ends.

## 4. Conformance suite

The package `ratelimit/storetest` contains one test suite for the contract.

```go
func TestRateLimitConformance(t *testing.T) {
    client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"})
    t.Cleanup(func() { _ = client.Close() })

    storetest.Run(t, func(t *testing.T) ratelimit.Store {
        namespace := fmt.Sprintf("test-%d", time.Now().UnixNano())
        store, err := redisstore.NewRateLimitStore(client, redisstore.WithNamespace(namespace))
        if err != nil {
            t.Fatal(err)
        }
        return store
    })
}
```

`Run` calls the function one time for each check. Each check uses its own name.
The name of each check starts with the contract rule that it proves, for example `RS4`.

The suite has checks for RS1 to RS7:

- The full start, the limits of a grant, the refill by the store time, the limit of the burst, the returned tokens, the isolation of names, the store time, and an ended context.
- A concurrency check: 32 concurrent operations get exactly the tokens of the bucket.

The suite cannot control the clock of a real store. The refill check waits 20 ms and then compares the grant with the store time of the two responses.

The suite has no check for RS8 and RS9. The tests of each adapter check these rules.

The tests of the suite run it against the in-memory store and against 14 broken stores. Each broken store must fail a check, and each check must reject a broken store.

## 5. Redis store

```go
store, err := redisstore.NewRateLimitStore(client, redisstore.WithNamespace("production"))
```

`NewRateLimitStore` accepts each client of `go-redis` that can run scripts.
It uses the options of the Redis adapter of the circuit breaker: `WithNamespace` is required, and `WithKeyPrefix` has the default `faultvane`.
It returns an error that matches `faultvane.ErrInvalidConfig` when the client is `nil`, or when the namespace or the key prefix is not valid.

### Key

Each name uses one key:

| Key | Type |
|---|---|
| `<prefix>:{<namespace>:<name>}:rl:bucket` | Hash |

Example: `faultvane:{production:payment-api}:rl:bucket`

The braces are a hash tag. The circuit breaker keys of the same namespace and name are in the same hash slot.

| Field | Content |
|---|---|
| `v` | The version of the layout. The value is `1`. |
| `tk` | The number of tokens. The value can have a fraction. |
| `ts` | The time of the last operation, in microseconds of Unix time, from the Redis clock. |

### Script

The file `store/redis/scripts/ratelimit_acquire.lua` contains the one script of the store.
Redis runs a script as one atomic operation.

1. The script reads the time with the Redis command `TIME`.
2. It reads `tk` and `ts`. When the key does not exist, the bucket is full and `ts` is the current time.
3. It does the steps of section 2.
4. It writes `v`, `tk`, and `ts`, and sets the time to live of the key with `PEXPIRE` to `StateTTL`.
5. It returns the grant and the time as text. Text keeps all digits of a large number.

The store sends the script again when Redis lost its script cache.

### Errors

A fault of Redis and a reply that is not valid return an error of this form:

```text
redisstore: rate limit acquire: faultvane: store unavailable: <cause>
```

The error matches `faultvane.ErrStoreUnavailable`. It also matches the error of the context when the context ended.

### Limits

- The tests use one Redis server without cluster mode and one Redis server with cluster mode on. The server with cluster mode owns all hash slots. The conformance suite and the key test use it through a cluster client.
- The conformance suite also runs against a cluster of 3 nodes, with rate limits on each node. No test moves the hash slot of a rate limit key, and no test uses a failover. The store uses one key for each name, so each operation is in one hash slot.
- After a failover to a replica that is behind, the bucket can hold older values. The bucket then holds at most `Burst` tokens. The guarantee of the guide starts again from that time.
- When the Redis clock goes back, the bucket gets no tokens until the clock reaches the stored time again.

## 6. SQL store

```go
store, err := sqlstore.NewRateLimitStore(db, sqlstore.PostgreSQL, sqlstore.WithNamespace("production"))
if err != nil {
    return err
}
if err := store.Migrate(ctx); err != nil {
    return err
}
```

`NewRateLimitStore` accepts a `*sql.DB` and the dialect `sqlstore.PostgreSQL` or `sqlstore.MySQL`.
It uses the options of the [SQL adapter](../stores/sql.md) of the circuit breaker: `WithNamespace` is required, and `WithTable` has the default `faultvane_rate_limits`.
It returns an error that matches `faultvane.ErrInvalidConfig` when the database handle is `nil`, or when the dialect, the namespace, or the table name is not valid.

| Method | Purpose |
|---|---|
| `Migrate(ctx)` | Creates the table when it does not exist. |
| `Schema()` | Returns the `CREATE TABLE` statement. Use it when a migration tool owns the schema. |
| `Cleanup(ctx)` | Deletes the rows that expired. Returns the number of rows. |

### Table

The store keeps one row for each name. The table is not the table of the circuit breaker.

```sql
CREATE TABLE IF NOT EXISTS faultvane_rate_limits (
    namespace   VARCHAR(64) NOT NULL,
    name        VARCHAR(64) NOT NULL,
    tokens      DOUBLE PRECISION NOT NULL,
    refilled_at BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL,
    PRIMARY KEY (namespace, name)
)
```

| Column | Content |
|---|---|
| `namespace` | The namespace of the store. |
| `name` | The name of the limiter. |
| `tokens` | The number of tokens. The value can have a fraction. |
| `refilled_at` | The time of the last operation, in microseconds of Unix time, from the database clock. |
| `expires_at` | The time after which the cleanup can delete the row, in microseconds of Unix time. |

### Operation

Each `Acquire` is one transaction:

1. Start a transaction with the default isolation level of the database.
2. Read `tokens`, `refilled_at`, and the database time with `SELECT ... FOR UPDATE`. This statement locks the row. A second operation on the same name waits until the first transaction ends.
3. Calculate the new bucket and the grant with `Bucket.Acquire`. This step runs in the instance.
4. Write `tokens`, `refilled_at`, and `expires_at` with `UPDATE`.
5. Commit.

When the row does not exist, the store inserts a full bucket and does the transaction again, one time.
PostgreSQL uses `INSERT ... ON CONFLICT DO NOTHING`. MySQL uses `INSERT IGNORE`.
When many instances make the first operation of a name at the same time, one insert creates the row and the other inserts do nothing. Then each instance takes its tokens under the row lock. Thus the instances get together exactly the tokens of one full bucket.

A transaction with a row lock is the atomic mechanism because the calculation runs in the instance. One SQL statement that contains the calculation would be a second implementation for each dialect.
An operation that the database does not commit changes nothing.

### Clock

The clock is the time of the database server: `clock_timestamp()` in PostgreSQL and `NOW(6)` in MySQL. The precision is 1 microsecond.
The store reads the time in the statement that locks the row.
The clocks of the instances have no effect on the shared rate.

### Cleanup

Each operation sets `expires_at` to the store time plus `StateTTL`.
The cleanup deletes each row whose `expires_at` is before the database time. It deletes the expired rows of all namespaces in the table.

The cleanup runs in two ways:

- After a successful `Acquire`, when the last cleanup of this store value was 1 minute ago or more. The store ignores an error of this cleanup. The cleanup uses the context of the operation.
- When the application calls `Cleanup(ctx)`.

The store starts no goroutine for the cleanup.

### Errors

A fault of the database returns an error of this form:

```text
sqlstore: rate limit acquire: faultvane: store unavailable: <cause>
```

The error matches `faultvane.ErrStoreUnavailable`. It also matches the error of the context when the context ended.
The errors of `Migrate` and `Cleanup` start with `sqlstore: migrate:` and `sqlstore: cleanup:`.

### Measured latency

| Database | Median | 99th percentile |
|---|---|---|
| PostgreSQL | 1.0 ms | 1.6 ms |
| MySQL | 2.3 ms | 4.9 ms |

The measurement is `BenchmarkRateLimitAcquire` in `store/sql/ratelimit_test.go` with 2000 sequential operations of one client on one name. Each database was in a local container on the machine of [Benchmarks](../benchmarks.md).
A network between the application and the database adds its round-trip time for each of the four statements of the transaction.

### Limits

- Concurrent operations on one name wait for the row lock. The store timeout of the limiter limits this wait. With the default synchronization interval, each instance makes 10 operations in one second on the row.
- The synchronization interval must be longer than the latency of one operation. The defaults of the limiter are a synchronization interval of 100 ms and a store timeout of 50 ms.
- The first operation of a name uses more statements: the read that finds no row, the insert, and the transaction again.
- When the database clock goes back, the bucket gets no tokens until the clock reaches the stored time again. A statement that waits for the row lock can also have an earlier time than the stored time. The store then uses the stored time. No token is lost.
- The tests run with PostgreSQL 17 and MySQL 8.4, each as one server. The project has no test with other versions, with other databases, or with a failover to a replica.
- After a failover to a replica that is behind, the bucket can hold older values. The bucket then holds at most `Burst` tokens.

## 7. MongoDB store

```go
store, err := mongostore.NewRateLimitStore(collection, mongostore.WithNamespace("production"))
if err != nil {
    return err
}
if err := store.EnsureIndexes(ctx); err != nil {
    return err
}
```

`NewRateLimitStore` accepts a collection of the driver `go.mongodb.org/mongo-driver/v2`.
It uses the option of the [MongoDB adapter](../stores/mongodb.md) of the circuit breaker: `WithNamespace` is required.
It returns an error that matches `faultvane.ErrInvalidConfig` when the collection is `nil` or when the namespace is not valid.

`EnsureIndexes` creates the TTL index `faultvane_expire` on the field `expireAt`. This is the index of the circuit breaker adapter.
The store can use the collection of the circuit breaker adapter or a different collection.

### Document

The store keeps one document for each name.

| Field | Type | Content |
|---|---|---|
| `_id` | String | `<namespace>:<name>:rl:bucket` |
| `version` | Integer | A number that increases by 1 with each write of the bucket. The compare-and-set uses it. |
| `tokens` | Double | The number of tokens. The value can have a fraction. |
| `at` | Date | The time of the last operation, from the MongoDB clock. |
| `expireAt` | Date | The time after which MongoDB can remove the document. |
| `now` | Date | The MongoDB server time of the last read step of an operation. |

Example of an identifier: `production:payment-api:rl:bucket`

The namespace and the name cannot contain the separator `:`. Thus the document of a limiter and the document of a circuit breaker with the same namespace and name have different identifiers.

### Operation

Each `Acquire` is an optimistic compare-and-set:

1. **Read step.** `findOneAndUpdate` with an update pipeline and `upsert`. It writes the server time `$$NOW` to the field `now`. When the document does not exist, it creates a full bucket with the version 0. It returns the document.
2. Calculate the new bucket and the grant with `Bucket.Acquire`. This step runs in the instance.
3. **Write step.** `updateOne` with a filter on `_id` and on the `version` of the read step. It writes `tokens`, `at`, the version plus 1, and `expireAt`.
4. When the write step matches no document, a different instance changed the bucket in the meantime. The store starts again at step 1.

The store makes at most 16 attempts for one operation. After 16 lost attempts, the operation returns a store fault and removes no token.
A duplicate key error of two concurrent first operations on a new name also uses one attempt.
Only one write step can match one version. Thus concurrent operations, also the first operations of a new name, get together exactly the tokens of the bucket.

The compare-and-set is the atomic mechanism because the calculation runs in the instance. It is also the mechanism of the circuit breaker adapter.
One update pipeline that contains the calculation would use one command, but it would be a second implementation of the calculation.
The store uses no multi-document transaction. It does not need a replica set.

### Clock

The clock is the MongoDB variable `$$NOW`, the time of the MongoDB server. The precision is 1 millisecond.
A part of a token stays in the bucket, so the precision does not change the rate.
The clocks of the instances have no effect on the shared rate.

### Cleanup

Each operation sets `expireAt` to the store time plus `StateTTL`. The read step gives a new document the same expiry time.
The TTL monitor of MongoDB removes a document after its `expireAt` time. The TTL monitor runs at an interval, so a document can stay for some time after its `expireAt` time.
Without the index of `EnsureIndexes`, MongoDB does not remove old documents.

Call `EnsureIndexes` before the application gets traffic. The call also creates the collection. Without it, the first write creates the collection, and that write can be slower than the store timeout.

### Errors

A fault of MongoDB, a document that is not a bucket, and the end of the 16 attempts return an error of this form:

```text
mongostore: rate limit acquire: faultvane: store unavailable: <cause>
```

The error matches `faultvane.ErrStoreUnavailable`. It also matches the error of the context when the context ended.
The error of `EnsureIndexes` starts with `mongostore: ensure indexes:`.

### Measured latency

| Median | 99th percentile |
|---|---|
| 0.30 ms | 0.87 ms |

The measurement is `BenchmarkRateLimitAcquire` in `store/mongodb/ratelimit_test.go` with 2000 sequential operations of one client on one name. MongoDB was in a local container on the machine of [Benchmarks](../benchmarks.md).
A network between the application and MongoDB adds its round-trip time for each of the two commands.

### Limits

- An operation without contention uses two commands. Each lost race adds two commands.
- An operation can lose a race one time for each other instance that writes the same bucket at the same time. More than 16 instances that synchronize one name at the same instant can use all 16 attempts. The operation then returns a store fault. The limiter counts it as one failed synchronization, and the tokens that it gave back in this operation are lost.
- When the context ends between the read step and the write step, the operation changes no tokens. The read step can leave a new document with a full bucket. That is equal to no document.
- When the MongoDB clock goes back, the bucket gets no tokens until the clock reaches the stored time again.
- The store uses the write settings of the collection that the application supplies. With a write concern that does not wait for the majority of a replica set, a failover can lose the last operations. The bucket then holds at most `Burst` tokens.
- The tests run with MongoDB 7 as one server. The project has no test with a replica set or a sharded cluster.
