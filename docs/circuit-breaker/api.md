# API

This document describes the planned public API of the circuit breaker.
The API can change before version 1.0.

## 1. Create a breaker

```go
store := redisstore.New(redisClient, redisstore.WithNamespace("production"))

breaker, err := circuitbreaker.New("payment-service",
    circuitbreaker.WithStore(store),
    circuitbreaker.WithFailureRateThreshold(0.5),
    circuitbreaker.WithMinimumCalls(20),
    circuitbreaker.WithOpenDuration(30*time.Second),
)
if err != nil {
    return err
}
defer breaker.Close(ctx)
```

- `New` validates the configuration. An invalid configuration returns `faultvane.ErrInvalidConfig`.
- A breaker without a store is a local breaker. It starts no goroutine.
- A breaker with a store owns one synchronizer goroutine. `breaker.Close` stops it.
- The store is an adapter. `redisstore` is the Redis adapter. [Stores](../stores/README.md) lists the adapters and the contract for a new adapter.
- The application supplies the client of the store, for example the Redis client or the `*sql.DB`. TLS, authentication, and connection timeouts are settings of that client.

## 2. Run a call

Three forms are available. They have the same behavior.

### Function with a result

```go
receipt, err := circuitbreaker.Execute(ctx, breaker, func(ctx context.Context) (Receipt, error) {
    return paymentClient.Charge(ctx, order)
})
```

`Execute` is a generic function, because a Go method cannot have a type parameter.

### Function without a result

```go
err := breaker.Do(ctx, func(ctx context.Context) error {
    return paymentClient.Ping(ctx)
})
```

### Permit

Use a permit when a function does not fit, for example in middleware or with streaming calls.

```go
permit, err := breaker.Allow(ctx)
if err != nil {
    return err
}
response, err := send(request)
if err != nil {
    permit.Failure()
    return err
}
permit.Success()
```

Each permit must get exactly one result: `Success`, `Failure`, or `Ignore`.
The permit measures the duration from `Allow` to the result.
A permit is a small value. `Allow` does not allocate memory in the `CLOSED` state.

## 3. Errors

| Error | Meaning | Source |
|---|---|---|
| `circuitbreaker.ErrCircuitOpen` | The circuit is `OPEN`. The breaker did not call the dependency. | `Execute`, `Do`, `Allow` |
| `circuitbreaker.ErrProbeNotPermitted` | The circuit tests the dependency, and this call did not get a probe lease. | `Execute`, `Do`, `Allow` |
| `faultvane.ErrRejected` | The parent of all rejections. Both errors above match it with `errors.Is`. | Match only |
| `faultvane.ErrInvalidConfig` | The configuration is not valid. The message names the option. | `New` |
| `timeout.ErrTimeout` | The timeout policy stopped the call. The breaker counts it as a failure. | Timeout policy |
| `faultvane.ErrStoreUnavailable` | The store did not answer. | Store adapters and events only |
| The error of the dependency | The breaker returns it without a change. | `Execute`, `Do` |

Rules:

- The breaker never wraps or replaces the error of the dependency.
- A rejection always matches `faultvane.ErrRejected`. An error of the dependency never matches it.
- `Execute`, `Do`, and `Allow` never return `faultvane.ErrStoreUnavailable`. A store fault changes the mode of the breaker. It does not fail a call.

```go
result, err := circuitbreaker.Execute(ctx, breaker, call)
switch {
case err == nil:
    return result, nil
case errors.Is(err, faultvane.ErrRejected):
    return fallback(), nil
default:
    return zero, err
}
```

## 4. Classify results

```go
circuitbreaker.WithClassifier(func(err error) circuitbreaker.Result {
    if errors.Is(err, ErrNotFound) {
        return circuitbreaker.Success
    }
    return circuitbreaker.DefaultClassifier(err)
})
```

[Failure detection](failure-detection.md) defines the default rule.

## 5. Read the status

| Method | Result |
|---|---|
| `breaker.Name()` | The name of the breaker. |
| `breaker.State()` | `Closed`, `Open`, or `HalfOpen`, from the snapshot or the local state. |
| `breaker.Mode()` | `Distributed` or `Local`. |

These methods read memory only.
Use them for health endpoints and diagnostics.
Do not use them to decide if a call is permitted. Use `Allow` for that.

## 6. Events

```go
circuitbreaker.WithObserver(observer)
```

The observer gets an event for each transition, each change of mode, each reconciliation, and each store fault.
The request path does not call the observer for each call. Call metrics come from counters.
[Observability](observability.md) lists the events and the ready-made observers.

## 7. Context rules

- Each function that can block accepts a `context.Context`.
- The breaker passes the context to the call without a change.
- When the caller cancels the context before the permit decision, the function returns the context error and records nothing.
- A store operation on the request path uses the shorter of the store timeout and the deadline of the context.

## 8. Concurrency rules

- All methods of a breaker are safe for concurrent use.
- Create one breaker for each dependency and use it again. Do not create a breaker for each request.
- Two breakers with the same name and the same store in one process are not permitted. `New` returns an error.
