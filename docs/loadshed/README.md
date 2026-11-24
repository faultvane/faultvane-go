# Load shedding

The load shedding policy protects the service itself.
When the service is overloaded, the policy rejects low-priority work first.

The [requirements](requirements.md) list what the policy must do. Each requirement has an identifier.

## 1. Purpose

A service that accepts more work than it can do becomes slow for all requests.
Load shedding rejects a part of the work early, before the work uses resources.
The important work then continues at normal speed.

One instance of the policy is a shedder.
A shedder counts the calls in progress that go through it.
The utilization is the number of calls in progress divided by a configured maximum.
Each call has a priority. Each priority has a threshold.
The shedder rejects a call when the utilization is at or above the threshold of its priority.

## 2. API

The package is `github.com/faultvane/faultvane-go/loadshed`.

| Item | Function |
|---|---|
| `New(opts ...Option) (*Shedder, error)` | Makes a shedder. It returns an error when the configuration is invalid. |
| `(*Shedder).Do(ctx, call) error` | Runs the call when the shedder admits it. A `Shedder` is a `faultvane.Policy`. |
| `(*Shedder).InFlight() int` | Returns the number of calls in progress. |
| `Priority` | The type of a priority. The levels are `Critical`, `High`, `Normal`, and `Low`. |
| `WithPriority(ctx, p) context.Context` | Returns a context that has the priority `p`. |
| `PriorityFrom(ctx) Priority` | Returns the priority of a context. |
| `ErrShed` | The error of a rejected call. It matches `faultvane.ErrRejected`. |

### Protect a handler

```go
shedder, err := loadshed.New(loadshed.WithMaxInFlight(200))
if err != nil {
    return err
}

err = shedder.Do(ctx, func(ctx context.Context) error {
    return handle(ctx, request)
})
if errors.Is(err, loadshed.ErrShed) {
    return errOverloaded
}
```

### Set the priority of a call

Set the priority where the application knows the type of the work, for example in a middleware.

```go
ctx = loadshed.WithPriority(ctx, loadshed.Critical)
err = shedder.Do(ctx, payOrder)
```

```go
ctx = loadshed.WithPriority(ctx, loadshed.Low)
err = shedder.Do(ctx, refreshRecommendations)
```

A context without a priority has the priority `Normal`.
A value that is not one of the four levels is also `Normal`.

### Read the load

```go
inProgress := shedder.InFlight()
```

## 3. Options

| Option | Default | Valid values | Function |
|---|---|---|---|
| `WithMaxInFlight(n int)` | 100 | 1 or more | The largest number of calls in progress. |
| `WithThreshold(p Priority, utilization float64)` | See the next table | Above 0 and at most 1 | The utilization at or above which the shedder rejects the calls of priority `p`. |

Default thresholds:

| Priority | Threshold | Rejected with the default maximum of 100 |
|---|---|---|
| `Low` | 0.5 | At 50 or more calls in progress |
| `Normal` | 0.75 | At 75 or more calls in progress |
| `High` | 0.9 | At 90 or more calls in progress |
| `Critical` | 1.0 | At 100 calls in progress |

Validation rules:

- The maximum must be 1 or more.
- Each threshold must be above 0 and at most 1.
- The priority of `WithThreshold` must be one of the four levels.
- The thresholds must not decrease from `Low` to `Critical`. Two priorities can have the same threshold.

`New` examines the thresholds after it applies all options.
Thus the order of the options is not important.

An invalid value makes `New` return an error that matches `faultvane.ErrInvalidConfig`.
The error message names the option and the rule, for example `faultvane: invalid configuration: Threshold must not decrease from Low to Critical`.

## 4. Behavior rules

1. A call is in progress from the time the shedder admits it until the call returns or panics.
2. The shedder makes the decision one time, before the call starts. It does not stop a call that is in progress.
3. The shedder rejects a call when the utilization is at or above the threshold of the priority of the call.
4. With no call in progress, the shedder admits a call of each priority.
5. The number of calls in progress never exceeds the maximum. This is also true when many goroutines start calls at the same time.
6. With the default thresholds, the shedder rejects `Critical` work only when the number of calls in progress is at the maximum.
7. A rejected call does not run. The shedder does not count it as a call in progress.
8. `Do` returns the error of the call without a change.
9. When the context has ended before the call starts, `Do` returns the error of the context and runs nothing.
10. A panic of the call continues to the caller with the same value. The call stops being in progress.
11. The shedder starts no goroutine.
12. All methods are safe for concurrent use.

### Select the maximum

The maximum is the number of calls that the service can do at the same time without a decrease of speed.
Measure this number with a load test. Do not estimate it.
With a maximum that is too high, the shedder does not protect the service.
With a maximum that is too low, the shedder rejects work that the service can do.

## 5. Interaction with the other policies

[Policy composition](../policy-composition.md) gives the recommended order of the policies.
Load shedding is the outermost policy.

```go
pipeline := faultvane.Combine(shedder, limiter, breaker)
```

| Policy | Interaction |
|---|---|
| Rate limiter | It is inside load shedding. Work that the shedder rejects uses no token of the rate limiter. |
| Bulkhead and concurrency limiter | They limit the calls to one dependency. Load shedding limits the work of the full service. Use one shedder for the service and one bulkhead for each dependency. |
| Circuit breaker | It is inside load shedding. A rejection of the shedder does not reach the breaker, so the breaker does not count it as a failure. |
| Retry | A retry policy must not try again after `faultvane.ErrRejected`. A retry of rejected work adds load to an overloaded service. |

All policies inside the shedder run while the call is in progress.
Thus the time of retries and of slow dependencies adds to the load that the shedder measures.

## 6. Limits

- The only load measure is the number of calls in progress through one shedder. The shedder does not measure CPU use, memory use, latency, or queue length.
- The shedder counts only the work that goes through it. Use one shedder for all the work that uses the same resources.
- The shedder has no queue. The application decides what to do after a rejection.
- The maximum is fixed. It does not adapt to the measured latency.
- With a small maximum, some thresholds give the same number of calls. For example, with a maximum of 1, the shedder admits each priority only when no call is in progress.
- A nested call through the same shedder counts as two calls in progress.
- All goroutines change one counter. The benchmark `BenchmarkDoAdmittedParallel` in `loadshed/bench_test.go` measures the cost of this contention.
