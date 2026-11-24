# Faultvane

Faultvane is a resilience library for Go services.
Its first component is a distributed circuit breaker.
Many application instances share one breaker state through Redis.
Each instance continues to work when Redis is not available.

> **Status: design phase.**
> The repository contains the design and the plan.
> It does not contain an implementation yet.
> The API in this document is the planned API. It can change before version 1.0.

## Why Faultvane

An in-process circuit breaker keeps a separate state in each application instance.
With 10 instances, each instance sees only a small part of the failures.
The dependency can be fully down, and no instance reaches its failure threshold.
The circuit opens late, and the unhealthy dependency continues to receive traffic.

Faultvane solves this problem with these properties:

- **Shared state.** All instances add their results to one failure window in Redis. The circuit opens for all instances together.
- **No Redis call on the request path.** Each instance makes the decision from local memory. A background task synchronizes with Redis in batches.
- **Redis is not a single point of failure.** When Redis is not available, each instance uses a local breaker for a limited time.
- **Safe recovery.** Each state has a generation number. A stale instance cannot overwrite a newer state.
- **Coordinated recovery probes.** In the `HALF_OPEN` state, only a configured number of probes go to the dependency across all instances. The default is one probe.

## Planned components

| Component | Purpose | Status |
|---|---|---|
| Core | Shared errors, clock, events, policy composition | Planned |
| Circuit breaker | Distributed breaker with Redis state and local fallback | Planned, first |
| Timeout | Time limit for each attempt | Planned |
| Retry | Retries with backoff, jitter, and a retry budget | Planned |
| Bulkhead | Isolation of resources for each dependency | Planned |
| Concurrency limiter | Limit on calls that run at the same time | Planned |
| Rate limiter | Limit on calls in a time period | Planned |
| Load shedding | Rejection of low-priority work under overload | Planned |

The [roadmap](docs/roadmap.md) gives the order and the exit criteria of each milestone.

## Planned usage

```go
store := redisstore.New(redisClient, redisstore.WithNamespace("production"))
defer store.Close(ctx)

breaker, err := circuitbreaker.New("payment-service",
    circuitbreaker.WithStore(store),
    circuitbreaker.WithFailureRateThreshold(0.5),
    circuitbreaker.WithMinimumCalls(20),
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

The application does not use Redis keys, generations, or probe leases.
The [API document](docs/api.md) gives the full planned API.

## Documentation

| Document | Content |
|---|---|
| [Documentation index](docs/README.md) | The list of all documents |
| [Requirements](docs/requirements.md) | Requirements and acceptance tests with identifiers |
| [Roadmap](docs/roadmap.md) | Milestones, exit criteria, and definition of done |
| [Architecture](docs/architecture.md) | Components, request path, synchronization, module layout |
| [State machine](docs/circuit-breaker/state-machine.md) | States, transitions, and generation rules |
| [Redis data model](docs/circuit-breaker/redis-data-model.md) | Keys, fields, atomic scripts, and TTL |
| [Degraded mode](docs/circuit-breaker/degraded-mode.md) | Behavior without Redis, reconciliation, consistency model |
| [Decision records](docs/adr/README.md) | The important design decisions and their alternatives |

## Requirements

- Go 1.24 or later.
- Redis 6.2 or later, or a compatible server. Standalone, Sentinel, and Cluster deployments are in scope.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a change.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes.

## License

The license is not selected yet.
Until the repository contains a `LICENSE` file, all rights are reserved.
