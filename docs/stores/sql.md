# SQL adapter

This document describes the SQL adapter.
The [store contract](README.md) defines the rules for all adapters.
This document defines how the SQL adapter satisfies them: the table, the transactions, the clock, and the cleanup.
After version 1.0, this data model is a compatibility commitment.

| Item | Value |
|---|---|
| Module | `github.com/faultvane/faultvane-go/store/sql` |
| Package name | `sqlstore` |
| Minimum Go version | 1.25 |
| API | `database/sql`. The application selects the driver. |
| Dialects | `sqlstore.PostgreSQL` and `sqlstore.MySQL` |

## 1. Setup

```go
db, err := sql.Open("pgx", dsn)
if err != nil {
    return err
}

store, err := sqlstore.New(db, sqlstore.PostgreSQL, sqlstore.WithNamespace("production"))
if err != nil {
    return err
}
if err := store.Migrate(ctx); err != nil {
    return err
}

breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(store))
if err != nil {
    return err
}
defer breaker.Close(ctx)
```

The application imports the driver, for example `github.com/jackc/pgx/v5/stdlib` or `github.com/go-sql-driver/mysql`.
The adapter module does not import a driver. Only its tests do.

`New` returns an error that matches `faultvane.ErrInvalidConfig` in these conditions:

- The database handle is `nil`.
- The dialect is not `PostgreSQL` or `MySQL`.
- The namespace is not set, or it is not valid.
- The table name is not valid.

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names. It has 1 to 64 characters from `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`. |
| `WithTable(table)` | `faultvane_circuit_breakers` | The name of the table. It has 1 to 64 characters from `a-z`, `A-Z`, `0-9`, `_`. |

| Method | Purpose |
|---|---|
| `Migrate(ctx)` | Creates the table when it does not exist. |
| `Schema()` | Returns the `CREATE TABLE` statement. Use it when a migration tool owns the schema. |
| `Cleanup(ctx)` | Deletes the rows that expired. Returns the number of rows. |

## 2. Table

The adapter keeps one row for each breaker.

```sql
CREATE TABLE IF NOT EXISTS faultvane_circuit_breakers (
    namespace  VARCHAR(64) NOT NULL,
    name       VARCHAR(64) NOT NULL,
    record     TEXT NOT NULL,
    expires_at BIGINT NOT NULL,
    PRIMARY KEY (namespace, name)
)
```

| Column | Content |
|---|---|
| `namespace` | The namespace of the adapter. |
| `name` | The name of the breaker. |
| `record` | The `circuitbreaker.Record` of the breaker as JSON text. |
| `expires_at` | The time after which the cleanup can delete the row. It is the database time in microseconds since the Unix epoch. |

The record contains the state, the generation, the transition time, the open duration, the number of probe successes, the leases, the window buckets, and the configuration fingerprint of the last writer.
The JSON field names are the names of the Go fields, for example `State`, `Generation`, `Leases`, `Buckets`, and `Fingerprint`.
A record without the field `Fingerprint` is valid. Its fingerprint is the empty text. A row from before the fingerprint needs no migration.
A new row has the record `{}`. That is `CLOSED` with generation 0.
The row has no schema version column.

## 3. Operations

`Report`, `AcquireProbe`, `CompleteProbe`, and `Restore` each use one transaction:

1. Start a transaction with the default isolation level of the database.
2. Read the record and the database time with `SELECT ... FOR UPDATE`. This statement locks the row.
3. Apply the event to the record with the method of `circuitbreaker.Record`, through `Record.Apply`. This step runs in the instance.
4. Write the record and the new `expires_at` with `UPDATE`.
5. Commit.

When the row does not exist, the adapter inserts an empty row and does the transaction again, one time.
PostgreSQL uses `INSERT ... ON CONFLICT DO NOTHING`. MySQL uses `INSERT IGNORE`.

`Load` is one `SELECT` without a lock and without a transaction.
It returns the record and the database time. It writes nothing.
A row that does not exist gives `CLOSED` with generation 0.

Each write operation writes the row and sets `expires_at`, also when the record ignores the event.

## 4. How the adapter satisfies the contract

| Rule | How |
|---|---|
| C1 Atomic | One transaction for each operation. A failed operation rolls back. |
| C2 Ordered | The row lock of `SELECT ... FOR UPDATE`. The operations on one breaker run in sequence. |
| C3 State machine | The methods of `circuitbreaker.Record`. The adapter contains no transition logic. |
| C4 Generation | The methods of `circuitbreaker.Record`. |
| C5 One clock | The time of the database server. See section 5. |
| C6 Leases | The record holds each lease with its end time in database time. |
| C7 Isolation | The primary key is the namespace and the name. |
| C8 Context | Each statement uses the context of the operation. The adapter repeats a transaction only one time, after the insert of a new row. |
| C9 Errors | Each error of the database and each record that is not valid JSON matches `faultvane.ErrStoreUnavailable`. The message starts with `sqlstore:` and the name of the operation. |
| C10 Cleanup | The column `expires_at` and a delete statement. See section 6. |
| C11 Data loss is visible | A row that does not exist gives `CLOSED` with generation 0. |
| C12 No side effects | The adapter starts no goroutine and has no lock in the process. The row lock is in the database and ends with the transaction. |
| C13 Fingerprint | The field `Fingerprint` of the record. The method `Record.Apply` gives the response with the fingerprint from before the operation. |

The adapter passes the conformance suite with PostgreSQL and with MySQL.

## 5. Clock

| Dialect | Expression | Precision |
|---|---|---|
| PostgreSQL | `clock_timestamp()` | Microseconds |
| MySQL | `NOW(6)` | Microseconds |

The adapter reads the time in the statement that locks the row.
All instances that use the same database server get the same clock.
The clocks of the instances have no effect on the shared state.

## 6. Cleanup

| Operation | Value of `expires_at` |
|---|---|
| `Report`, `AcquireProbe`, `CompleteProbe` | Database time plus the effective state TTL of the breaker |
| `Restore` | Database time plus 1 hour |

[Configuration](../circuit-breaker/configuration.md) defines the effective state TTL.

The cleanup deletes each row whose `expires_at` is before the database time.
The delete statement has no filter for the namespace. It deletes the expired rows of all namespaces in the table.

The cleanup runs in two ways:

- After a successful `Report`, when the last cleanup of this store value was 1 minute ago or more. The adapter ignores an error of this cleanup. The cleanup uses the context of the report.
- When the application calls `Cleanup(ctx)`.

The adapter starts no goroutine for the cleanup.
An application with no traffic on all breakers does no cleanup. Call `Cleanup` from a scheduled job when this matters.

## 7. Measured latency

| Database | Operation | Time for each operation |
|---|---|---|
| PostgreSQL | `Load` | 0.29 ms |
| PostgreSQL | `Report` | 0.86 ms |
| MySQL | `Load` | 0.26 ms |
| MySQL | `Report` | 2.0 ms |

The measurement used one client and sequential operations, with each database in a local container.
[Benchmarks](../benchmarks.md) gives the machine and the method.
A network between the application and the database adds its round-trip time for each statement of the transaction.

## 8. Limits

- A write operation is a transaction with a row lock. Concurrent operations on one breaker wait for the lock. The store timeout of the breaker limits this wait.
- The request path does not call the store in the `CLOSED` state, so the latency of the database does not change the latency of a normal call. It changes the lowest practical sync interval. The project did not measure this interval. The validation requires that `SyncInterval` is longer than `StoreTimeout`.
- The tests run with PostgreSQL 17 and MySQL 8.4. The project has no test with other versions or with other databases.
- The data loss handling is the same as for each adapter. When a row is lost, an instance calls `Restore` with its last authoritative snapshot. [Degraded mode](../circuit-breaker/degraded-mode.md) describes it.
- The adapter has no metrics of its own. `faultvaneotel.InstrumentStore` measures the duration of each operation.

## 9. Acceptance tests

The file `store/sql/acceptance_test.go` runs these acceptance tests of the [requirements](../circuit-breaker/requirements.md) against PostgreSQL and against MySQL.

| Test | Function | Method |
|---|---|---|
| AT-3 | `TestAT3_RequestsContinueWithoutTheStore` | The test stops the connection of one instance. The instance changes to local mode. All calls succeed. The local breaker opens after 2 failures. |
| AT-4 | `TestAT4_ReconciliationKeepsTheNewerState` | The test stops the connection of one instance. A second instance opens the circuit in the database. The test starts the connection again. The first instance takes the state `OPEN` with generation 1. |
| AT-5 | `TestAT5_StaleWriterIsRejected` | The database has `HALF_OPEN` with generation 100. Two instances get a probe. The probe failure of one instance gives `OPEN` with generation 101. Then the other instance sends a probe success with generation 100. The database keeps `OPEN` with generation 101. |
| AT-6 | `TestAT6_InstancesConvergeAfterTheOutage` | The test stops the connections of four instances. Two instances open locally and two stay closed. A report through a direct connection opens the circuit in the database. The test starts the connections again. All instances get `OPEN` with generation 1 in distributed mode. |

Each instance has its own connection pool.
The test gives the driver its own dial function.
To stop a connection, the test closes all TCP connections of the pool and refuses each new connection.
The test does not stop the database container.

The tests need the databases of `docker-compose.test.yml`.
`sh scripts/test-stores.sh` starts them.
