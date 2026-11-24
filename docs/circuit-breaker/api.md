# API

This document describes the public API of the circuit breaker.
The API can change before version 1.0.

Status: all parts of this document are implemented.

```go
import "github.com/faultvane/faultvane-go/circuitbreaker"
```

## 1. Create a breaker

A breaker without a store is a local breaker:

```go
breaker, err := circuitbreaker.New("payment-service",
    circuitbreaker.WithFailureRateThreshold(0.5),
    circuitbreaker.WithMinimumCalls(20),
    circuitbreaker.WithOpenDuration(30*time.Second),
)
if err != nil {
    return err
}
```

A breaker with a store shares its state with the other instances:

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

- `New` validates the configuration. An invalid configuration returns an error that matches `faultvane.ErrInvalidConfig`.
- A breaker without a store starts no goroutine.
- A breaker with a store reads the authoritative state in `New`, with the start timeout as the limit. A failed read does not fail `New`. The breaker then starts in local mode.
- A breaker with a store owns one synchronizer goroutine. `Close` stops it.
- The store is an adapter. `redisstore` is the Redis adapter. [Stores](../stores/README.md) lists the adapters and the contract for a new adapter.
- The application supplies the client of the store, for example the Redis client or the `*sql.DB`. TLS, authentication, and connection timeouts are settings of that client.
- [Configuration](configuration.md) lists all options.

## 2. Run a call

Three forms are available. They have the same behavior.

### Function with a result

```go
receipt, err := circuitbreaker.Execute(ctx, breaker, func(ctx context.Context) (Receipt, error) {
    return paymentClient.Charge(ctx, order)
})
```

`Execute` is a generic function, because a Go method cannot have a type parameter.

When the call panics, the breaker records a failure and panics again with the same value.

### Function without a result

```go
err := breaker.Do(ctx, func(ctx context.Context) error {
    return paymentClient.Ping(ctx)
})
```

`Do` makes the breaker a `faultvane.Policy`. Use it in `faultvane.Combine`.

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
| `faultvane.ErrInvalidConfig` | The configuration is not valid. The message names the option. | `New`, and the `New` function of each adapter |
| `faultvane.ErrStoreUnavailable` | A store operation failed. | The methods of a store adapter, and the field `Err` of an event |
| The context error | The context ended before the permit decision, or before `Close` completed. | `Execute`, `Do`, `Allow`, `Close` |
| The error of the dependency | The breaker returns it without a change. | `Execute`, `Do` |

Rules:

- The breaker never wraps or replaces the error of the dependency.
- A rejection always matches `faultvane.ErrRejected`. An error of the dependency never matches it.
- `Execute`, `Do`, `Allow`, and `Close` never return `faultvane.ErrStoreUnavailable`. A store fault changes the mode of the breaker. It does not fail a call.
- An error of a different policy in the pipeline, for example `timeout.ErrTimeout`, is an error of the call for the breaker. The default classifier counts it as a failure.

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

A classifier returns `circuitbreaker.Success`, `circuitbreaker.Failure`, or `circuitbreaker.Ignored`.
[Failure detection](failure-detection.md) defines the default rule.

## 5. Read the status

| Method | Result |
|---|---|
| `breaker.Name()` | The name of the breaker. |
| `breaker.State()` | `circuitbreaker.Closed`, `circuitbreaker.Open`, or `circuitbreaker.HalfOpen`, from the snapshot or the local state. |
| `breaker.Mode()` | `circuitbreaker.Distributed` or `circuitbreaker.Local`. A breaker without a store is always `Local`. |
| `breaker.Stats()` | A `circuitbreaker.Stats` value with the state, the mode, and the counters of this breaker. |
| `breaker.ConfigFingerprint()` | The configuration fingerprint of this breaker, for example `v1:c0183f3821e11064`. [Failure detection](failure-detection.md) defines it. |

`State` and `Mode` have a `String` method. The texts are `closed`, `open`, `half_open`, `distributed`, and `local`.

These methods read memory only.
Use them for health endpoints and diagnostics.
Do not use them to decide if a call is permitted. Use `Allow` for that.

### Stats

| Field | Meaning |
|---|---|
| `State`, `Mode` | The current state and mode. |
| `Successes`, `Failures`, `Ignored` | The results of the calls that the breaker permitted, probes included. |
| `SlowCalls` | The calls that were slow. |
| `RejectedOpen` | The calls that got `ErrCircuitOpen`. |
| `RejectedProbe` | The calls that got `ErrProbeNotPermitted`. |
| `ProbeSuccesses`, `ProbeFailures` | The results of probes. |
| `Opened`, `HalfOpened`, `Closed` | The state transitions that this instance observed, by new state. |
| `StoreErrors` | The store operations that failed. |
| `LocalModeEntries` | The number of changes to local mode. |
| `Reconciliations` | The number of reconciliations with the store. |
| `LocalModeExpired` | `true` while local mode is longer than the maximum time. |
| `ConfigMismatch` | `true` from the first store response with a different configuration fingerprint until 10 store responses in sequence have no different fingerprint. |

All counters start at 0 when the application creates the breaker. They only increase.
[Observability](observability.md) describes how the OpenTelemetry module exports them.

## 6. Events

```go
type Observer interface {
    Observe(Event)
}
```

```go
circuitbreaker.WithObserver(observer)
```

The observer gets an `Event` for each state change, each change of mode, each reconciliation, each failed store operation, the end of the local mode time limit, and the start and the end of a configuration difference.
A failed health check in local mode gives no event.

| Field of `Event` | Content |
|---|---|
| `Kind` | `StateChanged`, `ModeChanged`, `Reconciled`, `StoreFault`, `LocalModeExpired`, or `ConfigMismatchChanged` |
| `Breaker` | The name of the breaker |
| `From`, `To` | The state before and after |
| `Generation` | The generation after the event |
| `Mode` | The mode after the event |
| `Err` | The cause, for a store fault and for a change to local mode. In other events it is `nil`. |
| `ConfigMismatch` | For `ConfigMismatchChanged`: `true` when the difference starts, `false` when it ends. In other events it is `false`. |

A `StoreFault` event sets only `Kind`, `Breaker`, `Mode`, and `Err`. The other fields have their zero values.
In a `ConfigMismatchChanged` event, `From` and `To` are the state of the response of the store, and `Mode` is `Distributed`.

Rules:

- The breaker does not call the observer for each call. Call numbers come from `Stats`.
- The breaker calls the observer in the goroutine that causes the event, and it waits for the observer. The observer must return quickly.
- The observer must not run a call through the same breaker.
- When the observer panics, the breaker recovers and writes an error log.

[Observability](observability.md) lists the events and the logs.

## 7. Stop a breaker

```go
err := breaker.Close(ctx)
```

- `Close` stops the synchronizer goroutine and waits for its end.
- Then, in distributed mode, `Close` synchronizes with the store one time. It sends the pending counts as the last report. The store timeout and the context limit this operation. In local mode, `Close` sends nothing.
- `Close` returns the context error when the context ends before the goroutine stops. In all other conditions it returns `nil`.
- `Close` on a breaker without a store does nothing and returns `nil`.
- It is safe to call `Close` more than one time.
- `Close` does not change the permit decision. Calls after `Close` use the last state in memory. The breaker sends no reports and does not refresh its snapshot at the sync interval. A probe request still calls the store.

## 8. Context rules

- Each function that can block accepts a `context.Context`.
- The breaker passes the context to the call without a change.
- When the context ended before the permit decision, the function returns the context error and records nothing.
- A probe request to the store uses the shorter of the store timeout and the deadline of the context.
- The result of a probe goes to the store with the store timeout. It does not use the context of the call.

## 9. Concurrency rules

- All methods of a breaker are safe for concurrent use.
- Create one breaker for each dependency and use it again. Do not create a breaker for each request.
- The library does not check for two breakers with the same name and the same store in one process. Each of them is one more instance for the store: each has its own synchronizer and its own local counters.
