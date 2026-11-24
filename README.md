# Faultvane

[![CI](https://github.com/faultvane/faultvane-go/actions/workflows/ci.yml/badge.svg)](https://github.com/faultvane/faultvane-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/faultvane/faultvane-go.svg)](https://pkg.go.dev/github.com/faultvane/faultvane-go)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Faultvane is a resilience toolkit for Go services.
It gives you the policies that keep a service stable when a dependency is slow, fails, or gets too much traffic.
You can use each policy alone, or combine the policies in one pipeline.

> **Status: implemented, not released.**
> All seven policies are implemented and tested.
> The circuit breaker shares its state through Redis, a SQL database, or MongoDB.
> No release tag exists yet. The [roadmap](docs/roadmap.md) lists the open items.
> The API can change before version 1.0.

## Policies

| Policy | Package | Problem that it solves | Shared state between instances |
|---|---|---|---|
| [**Circuit breaker**](docs/circuit-breaker/README.md) | `circuitbreaker` | A dependency is down. Calls to it waste time and add load. | Optional |
| [**Timeout**](docs/timeout/README.md) | `timeout` | A call does not return. | No |
| [**Retry**](docs/retry/README.md) | `retry` | A call fails for a short time. A second attempt can succeed. | No |
| [**Bulkhead**](docs/bulkhead/README.md) | `bulkhead` | One slow dependency uses all resources of the service. | No |
| [**Concurrency limiter**](docs/concurrencylimit/README.md) | `concurrencylimit` | Too many calls run at the same time. | No |
| [**Rate limiter**](docs/ratelimit/README.md) | `ratelimit` | Too many calls start in a time period. | No |
| [**Load shedding**](docs/loadshed/README.md) | `loadshed` | The service itself is overloaded. | No |

All seven policies are implemented.
Only the circuit breaker has shared state.

## Principles

- **Each policy works alone.** A policy has no dependency on a different policy.
- **The policies combine.** One pipeline applies many policies in a defined order. The [policy composition](docs/policy-composition.md) document gives the recommended order.
- **Local first.** Each policy makes its decision from local memory. A normal call does not use the network.
- **Shared state is optional.** A policy that can coordinate instances, such as the circuit breaker, uses a shared store. The store can be Redis, a SQL database, or another system. An adapter connects the policy to the store.
- **The shared store is never a single point of failure.** When the store is not available, the policy continues with local state.
- **No required dependencies.** The core module uses only the Go standard library. Each integration is a separate module.
- **Tested.** The project requires a statement coverage of 98% or more, with unit, integration, concurrency, and fault tests. Continuous integration rejects a change below this value.

## Usage

### One policy

```go
breaker, err := circuitbreaker.New("payment-service",
    circuitbreaker.WithFailureRateThreshold(0.5),
    circuitbreaker.WithOpenDuration(30*time.Second),
)
if err != nil {
    return err
}

receipt, err := circuitbreaker.Execute(ctx, breaker, func(ctx context.Context) (Receipt, error) {
    return paymentClient.Charge(ctx, order)
})
if errors.Is(err, faultvane.ErrRejected) {
    return fallbackReceipt(order), nil
}
```

### Many policies

Each constructor validates its configuration and returns an error.
The first argument of `faultvane.Combine` is the outermost policy.

```go
limit, err := bulkhead.New(bulkhead.WithMaxConcurrent(50))
if err != nil {
    return err
}
retrier, err := retry.New(
    retry.WithMaxAttempts(3),
    retry.WithBudget(0.1),
    retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
)
if err != nil {
    return err
}
attempt, err := timeout.New(2 * time.Second)
if err != nil {
    return err
}
pipeline := faultvane.Combine(limit, breaker, retrier, attempt)

receipt, err := faultvane.Execute(ctx, pipeline, func(ctx context.Context) (Receipt, error) {
    return paymentClient.Charge(ctx, order)
})
```

The program in [`examples/pipeline`](examples/pipeline/main.go) runs this pipeline against a local HTTP server.

### A policy with shared state

```go
store, err := redisstore.New(redisClient, redisstore.WithNamespace("production"))
if err != nil {
    return err
}

breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(store))
if err != nil {
    return err
}
defer breaker.Close(ctx)
```

With a store, all instances of the service share one circuit.
The circuit opens when the instances together see sufficient failures.
No instance has to reach the threshold alone.
A breaker with a store owns one background goroutine. `breaker.Close` stops it.

The import path of the Redis adapter is `github.com/faultvane/faultvane-go/store/redis`. The name of its package is `redisstore`.
The program in [`examples/shared`](examples/shared/main.go) runs five instances against one Redis.

### Metrics and traces

The module `otel` exports the counters of a circuit breaker as OpenTelemetry metrics and adds attributes to the active span.

```go
registration, err := faultvaneotel.RegisterMetrics(meter, breaker)
if err != nil {
    return err
}
defer registration.Unregister()

pipeline := faultvane.Combine(faultvaneotel.Traced(breaker), attempt)
```

The other policies have no metrics, no logs, and no observer yet.

## Documentation

| Document | Content |
|---|---|
| [Documentation index](docs/README.md) | The list of all documents |
| [Requirements](docs/requirements.md) | The requirements for the toolkit and for all policies |
| [Architecture](docs/architecture.md) | The layers, the policy model, shared state, and the module layout |
| [Policy composition](docs/policy-composition.md) | The order of the policies and their interaction |
| [Testing](docs/testing.md) | The test levels, the coverage rule, and the quality gates |
| [Roadmap](docs/roadmap.md) | The milestones, their results, and the open items |
| [Circuit breaker](docs/circuit-breaker/README.md) | The design and the API of the circuit breaker |
| [Timeout](docs/timeout/README.md) | The guide for the timeout policy |
| [Retry](docs/retry/README.md) | The guide for the retry policy |
| [Bulkhead](docs/bulkhead/README.md) | The guide for the bulkhead |
| [Concurrency limiter](docs/concurrencylimit/README.md) | The guide for the concurrency limiter |
| [Rate limiter](docs/ratelimit/README.md) | The guide for the rate limiter |
| [Load shedding](docs/loadshed/README.md) | The guide for load shedding |
| [Stores](docs/stores/README.md) | The store interface, the adapters, and the contract for a new adapter |
| [Redis adapter](docs/stores/redis.md) | Keys, fields, atomic scripts, and TTL |
| [SQL adapter](docs/stores/sql.md) | The table, the transactions, and the cleanup for PostgreSQL and MySQL |
| [MongoDB adapter](docs/stores/mongodb.md) | The document, the compare-and-set, and the TTL index |
| [Decision records](docs/adr/README.md) | The important design decisions and their alternatives |
| [Benchmarks](docs/benchmarks.md) | Measured results |
| [Scenarios in Docker](docs/scenarios.md) | Tests with 10 pods and four real stores |

## Requirements

Each module names its minimum Go version in its `go.mod` file.

| Module | Import path | Minimum Go version | Other requirements |
|---|---|---|---|
| Core, all policies | `github.com/faultvane/faultvane-go` | 1.24 | None. The module uses only the standard library. |
| Redis adapter | `github.com/faultvane/faultvane-go/store/redis` | 1.26 | Redis 6.2 or later, or a compatible server |
| SQL adapter | `github.com/faultvane/faultvane-go/store/sql` | 1.25 | PostgreSQL or MySQL, and a `database/sql` driver |
| MongoDB adapter | `github.com/faultvane/faultvane-go/store/mongodb` | 1.25 | MongoDB 4.4 or later |
| OpenTelemetry | `github.com/faultvane/faultvane-go/otel` | 1.26 | The OpenTelemetry API |

The Go version of an adapter module comes from the client library of its store.
No release tag exists yet.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a change.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes.

## License

Copyright 2026 The Faultvane Authors.

Faultvane has the [Apache License 2.0](LICENSE).
