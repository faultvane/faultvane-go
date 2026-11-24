# Faultvane

[![CI](https://github.com/faultvane/faultvane-go/actions/workflows/ci.yml/badge.svg)](https://github.com/faultvane/faultvane-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/faultvane/faultvane-go.svg)](https://pkg.go.dev/github.com/faultvane/faultvane-go)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Faultvane is a resilience toolkit for Go services.
It gives you the policies that keep a service stable when a dependency is slow, fails, or gets too much traffic.
You can use each policy alone, or combine the policies in one pipeline.

> **Status: design phase.**
> The repository contains the design and the plan.
> It does not contain an implementation yet.
> The API in this document is the planned API. It can change before version 1.0.

## Policies

| Policy | Problem that it solves | Shared state between instances |
|---|---|---|
| **Circuit breaker** | A dependency is down. Calls to it waste time and add load. | Optional |
| **Timeout** | A call does not return. | No |
| **Retry** | A call fails for a short time. A second attempt can succeed. | No |
| **Bulkhead** | One slow dependency uses all resources of the service. | No |
| **Concurrency limiter** | Too many calls run at the same time. | No |
| **Rate limiter** | Too many calls start in a time period. | Under evaluation |
| **Load shedding** | The service itself is overloaded. | No |

The circuit breaker is the first policy in the [roadmap](docs/roadmap.md).
The other policies follow in the order that the roadmap gives.

## Principles

- **Each policy works alone.** A policy has no dependency on a different policy.
- **The policies combine.** One pipeline applies many policies in a defined order. The [policy composition](docs/policy-composition.md) document gives the recommended order.
- **Local first.** Each policy makes its decision from local memory. A normal call does not use the network.
- **Shared state is optional.** A policy that can coordinate instances, such as the circuit breaker, uses a shared store. The store can be Redis, a SQL database, or another system. An adapter connects the policy to the store.
- **The shared store is never a single point of failure.** When the store is not available, the policy continues with local state.
- **No required dependencies.** The core module uses only the Go standard library. Each integration is a separate module.
- **Tested for production.** The project requires a statement coverage of 98% or more, with unit, integration, concurrency, and fault tests. Continuous integration rejects a change below this value.

## Planned usage

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

```go
pipeline := faultvane.Combine(
    bulkhead.New(bulkhead.WithMaxConcurrent(50)),
    breaker,
    retry.New(retry.WithMaxAttempts(3), retry.WithBudget(0.1)),
    timeout.New(2*time.Second),
)

receipt, err := faultvane.Execute(ctx, pipeline, func(ctx context.Context) (Receipt, error) {
    return paymentClient.Charge(ctx, order)
})
```

### A policy with shared state

```go
store := redisstore.New(redisClient, redisstore.WithNamespace("production"))

breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(store))
```

With a store, all instances of the service share one circuit.
The circuit opens when the instances together see sufficient failures.
No instance has to reach the threshold alone.

## Documentation

| Document | Content |
|---|---|
| [Documentation index](docs/README.md) | The list of all documents |
| [Requirements](docs/requirements.md) | The requirements for the toolkit and for all policies |
| [Architecture](docs/architecture.md) | The layers, the policy model, shared state, and the module layout |
| [Policy composition](docs/policy-composition.md) | The order of the policies and their interaction |
| [Testing](docs/testing.md) | The test levels, the coverage rule, and the quality gates |
| [Roadmap](docs/roadmap.md) | Milestones, exit criteria, and definition of done |
| [Circuit breaker](docs/circuit-breaker/README.md) | The design of the circuit breaker |
| [Stores](docs/stores/README.md) | The store interface, the adapters, and the contract for a new adapter |
| [Decision records](docs/adr/README.md) | The important design decisions and their alternatives |

## Requirements

- Go 1.24 or later.
- No other system is necessary for local policies.
- For shared state with the Redis adapter: Redis 6.2 or later, or a compatible server.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a change.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes.

## License

Copyright 2026 The Faultvane Authors.

Faultvane has the [Apache License 2.0](LICENSE).
