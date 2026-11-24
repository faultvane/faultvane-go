# Changelog

This file records all important changes of the project.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

No release exists yet. This section lists what the repository contains.

### Added

- Scenario tests in Docker: 10 pods, a controllable dependency, and Redis, PostgreSQL, MySQL, and MongoDB.
- Correction: the rate limiter does not reject a call when a goroutine uses a clock value from before the last refill.
- Correction: a new row of the SQL adapter has an expiry time, so a cleanup does not remove it.
Core module `github.com/faultvane/faultvane-go`:

- Root package: `ErrRejected`, `ErrInvalidConfig`, `ErrStoreUnavailable`, `Clock`, `SystemClock`, `Policy`, `Combine`, and `Execute`.
- Package `faultvanetest`: a clock that a test controls.
- Package `circuitbreaker`: a circuit breaker with a time-based sliding window, failure rate, failure count, and slow call conditions, coordinated half-open probes with leases, generations, and a classifier.
- Package `circuitbreaker`: shared state through the `Store` interface, with a synchronizer, early reports, local mode, start policies, a time limit for local mode, reconciliation, restore after data loss, and `Close`.
- Package `circuitbreaker`: the exported type `Record` with the transitions, the type `Thresholds`, and the in-memory store `MemoryStore`.
- Package `circuitbreaker`: `Mode`, `Stats`, `Observer`, `Event`, and logs with `log/slog`.
- Package `circuitbreaker/storetest`: the conformance suite for store adapters.
- Package `timeout`: a time limit for each attempt, with `ErrTimeout`.
- Package `retry`: retries with backoff, a retry rule, a gate, and a retry budget.
- Package `bulkhead`: a fixed limit of concurrent calls with an optional wait queue, with `ErrFull`.
- Package `concurrencylimit`: a limit of concurrent calls that adapts to the results of the calls, with `ErrLimitExceeded`.
- Package `ratelimit`: a token bucket for one process, with `ErrRateLimited`.
- Package `loadshed`: load shedding by priority, with `ErrShed`.

Separate modules:

- `store/redis` (package `redisstore`): the Redis adapter with Lua scripts.
- `store/sql` (package `sqlstore`): the SQL adapter for PostgreSQL and MySQL, with `Migrate`, `Schema`, and `Cleanup`.
- `store/mongodb` (package `mongostore`): the MongoDB adapter, with `EnsureIndexes`.
- `otel` (package `faultvaneotel`): `RegisterMetrics`, `Traced`, and `InstrumentStore` for the circuit breaker.
- `examples`: the programs `pipeline` and `shared`, each with a test of its output.

Tests and tools:

- Coverage check with a minimum of 98% for each module and 95% for each package, enforced in continuous integration.
- Requirement traceability check.
- Store tests against Redis, PostgreSQL, MySQL, and MongoDB in containers, with `scripts/test-stores.sh`.
- A fuzz workflow that runs each day, and a system benchmark workflow that runs each week.
- Benchmarks for each policy, for the store operations, and for the system with 1 to 100 instances.

Documents:

- Requirements and a guide or design documents for each of the seven policies.
- Documents for the store contract and for the Redis, SQL, and MongoDB adapters.
- Decision records 0001 to 0007.
- Apache License 2.0.

### Not included

- A release tag.
- Metrics, logs, and an observer for the policies other than the circuit breaker.
- The configuration fingerprint and the transition notifications of the first design.
- A rate limit that many instances share.
