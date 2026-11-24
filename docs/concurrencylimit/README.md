# Concurrency limiter

The concurrency limiter limits the number of calls that run at the same time for one dependency.
It adapts the limit to the results of the calls.

Status: implemented in the package `concurrencylimit`.

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the limiter must do. Each requirement has an identifier. |

## 1. Purpose

The application does not always know how many concurrent calls a dependency can serve.
The capacity also changes with time.

The limiter finds the limit from the results of the calls.
It uses the AIMD rule: additive increase, multiplicative decrease.

- When a call shows overload, the limiter multiplies the limit by a ratio below 1. The limit decreases quickly.
- When calls succeed under load, the limiter adds a small value to the limit. The limit increases slowly.

## 2. API

```go
func New(opts ...Option) (*Limiter, error)

func (l *Limiter) Do(ctx context.Context, call func(context.Context) error) error
func (l *Limiter) Limit() int
func (l *Limiter) InFlight() int
func (l *Limiter) Stats() Stats

var ErrLimitExceeded error
```

| Item | Behavior |
|---|---|
| `New` | Creates a limiter. It returns an error that matches `faultvane.ErrInvalidConfig` when an option is not valid. |
| `Do` | Runs the call when the calls in progress are below the limit. After the call, it adapts the limit. It returns the error of the call without a change. |
| `Limit` | Returns the effective limit at this moment. |
| `InFlight` | Returns the number of calls in progress. |
| `Stats` | Returns the counts, the limit, and the calls in progress. |
| `ErrLimitExceeded` | The rejection error. It matches `faultvane.ErrRejected`. |

`*Limiter` implements `faultvane.Policy`.
All methods are safe for concurrent use.

### Example

```go
limiter, err := concurrencylimit.New(
    concurrencylimit.WithInitialLimit(20),
    concurrencylimit.WithMinLimit(5),
    concurrencylimit.WithMaxLimit(100),
    concurrencylimit.WithLatencyThreshold(200*time.Millisecond),
    concurrencylimit.WithBackoffRatio(0.8),
)
if err != nil {
    panic(err)
}

err = limiter.Do(context.Background(), search)
switch {
case errors.Is(err, concurrencylimit.ErrLimitExceeded):
    fmt.Println("the search service is at its limit")
case errors.Is(err, faultvane.ErrRejected):
    fmt.Println("a policy rejected the call")
case err != nil:
    fmt.Println("the call failed:", err)
}
```

### Example: select the overload signals

```go
limiter, err := concurrencylimit.New(
    concurrencylimit.WithOverloadIf(func(err error) bool {
        return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errTooManyRequests)
    }),
)
```

### Statistics and metrics

```go
stats := limiter.Stats()
```

`Stats` returns a value of the type `Stats`. All counts start at 0 for a new instance and only increase.

| Field of `Stats` | Content |
|---|---|
| `Permitted` | The number of calls that the limiter permitted. |
| `Rejected` | The number of calls that returned `ErrLimitExceeded`. |
| `Decreases` | The number of overload signals. Each overload signal decreases the limit, or keeps it at `MinLimit`. |
| `Limit` | The effective limit at this moment. |
| `InFlight` | The number of calls in progress at this moment. |

A call whose context has ended before the decision changes no count.
Each count is one atomic value. To count makes no memory allocation and uses no lock.

The module `github.com/faultvane/faultvane-go/otel` exports these values as OpenTelemetry metrics:

```go
registration, err := faultvaneotel.RegisterConcurrencyLimitMetrics(meter, "payment", limiter)
```

| Metric | Type | Attributes | Content |
|---|---|---|---|
| `faultvane.concurrency_limit.calls` | Counter | `result`: `permitted` or `rejected` | `Permitted` and `Rejected` |
| `faultvane.concurrency_limit.limit_decreases` | Counter | None | `Decreases` |
| `faultvane.concurrency_limit.limit` | Gauge | None | `Limit` |
| `faultvane.concurrency_limit.in_flight` | Gauge | None | `InFlight` |

Each metric also has the attribute `faultvane.policy.name`. Its value is the name that you give to the function. Use a small, fixed set of names.
The metrics read `Stats` when the reader collects them. They add no work to a call.
`registration.Unregister()` removes the metrics.
The policy writes no logs, because it has no state transitions and no faults. It has no observer.

## 3. Options

| Option | Default | Valid values | Meaning |
|---|---|---|---|
| `WithInitialLimit(n)` | `20` | From `MinLimit` to `MaxLimit` | The limit of a new limiter. |
| `WithMinLimit(n)` | `1` | 1 or more | The limit never decreases below this value. |
| `WithMaxLimit(n)` | `200` | `MinLimit` or more | The limit never increases above this value. |
| `WithLatencyThreshold(d)` | Off | Above 0 | A call that takes this time or longer is an overload signal. |
| `WithBackoffRatio(r)` | `0.9` | Above 0 and below 1 | The limiter multiplies the limit by this value after an overload signal. |
| `WithOverloadIf(f)` | An error that matches `context.DeadlineExceeded` | Not nil | The function that tells if the error of a call is an overload signal. It also receives `nil`. |
| `WithClock(c)` | `faultvane.SystemClock()` | Not nil | The clock that measures the latency. Tests use a fake clock. |

When you change `MinLimit` or `MaxLimit`, make sure that `InitialLimit` stays in the range.
For example, `WithMaxLimit(10)` alone is not valid, because the default `InitialLimit` is 20.

To get a fixed limit, set `MinLimit`, `InitialLimit`, and `MaxLimit` to the same value.

## 4. Behavior rules

### Decision

1. If the context has already ended, `Do` returns the error of the context. It runs nothing.
2. If the number of calls in progress is equal to or above the limit, `Do` returns `ErrLimitExceeded` immediately. The limiter has no queue.
3. If not, the call runs.

The limiter keeps the limit as a number with a fraction.
The effective limit is the integer part. A limit of 2.9 permits 2 calls in progress.

When the limit decreases below the number of calls in progress, these calls continue.
The limiter rejects new calls until the number is below the limit.

### Adaptation

The limiter adapts the limit after each call that it ran.

| Result of the call | Change of the limit |
|---|---|
| Overload signal | `limit = max(MinLimit, limit × BackoffRatio)` |
| No overload signal, and the call was busy | `limit = min(MaxLimit, limit + 1/limit)` |
| No overload signal, and the call was not busy | None |
| Panic | None |

A call is an **overload signal** in two conditions:

- `OverloadIf` returns `true` for the error of the call.
- `LatencyThreshold` is set, and the call took this time or longer.

A call is **busy** when it starts while the calls in progress, with this call included, are at least half of the effective limit.
This rule prevents an increase without evidence.
A service with little traffic does not prove that the dependency can serve more concurrent calls.

The increase is `1/limit` for each call.
Thus the limit increases by approximately 1 after `limit` busy calls without an overload signal.

An error that is not an overload signal is a normal result.
For example, a "not found" error does not show overload.
A cancellation by the caller (`context.Canceled`) is also not an overload signal by default.

A rejection by the limiter does not change the limit.

### Other rules

- The limiter returns the error of the call without a change.
- When the call panics, the call is not in progress after that. The panic continues with the same value.
- The limiter starts no goroutine and has no `Close` method.
- The limiter does not stop a call in progress. Use a timeout for that.

## 5. Concurrency limiter or bulkhead

Both policies limit the calls in progress for one dependency.
Use one of them for one dependency, not both.

| Subject | Concurrency limiter | Bulkhead |
|---|---|---|
| Limit | Adaptive. The limiter changes it between a minimum and a maximum. | Fixed. The application sets it. |
| Input of the decision | The number of calls in progress, and the results of the last calls. | The number of calls in progress. |
| Waiting | None. | Optional, with a limit of callers and of time. |
| Purpose | Find the number of concurrent calls that the dependency can serve. | Protect the resources of the service from one slow dependency. |
| Use it when | You do not know the capacity of the dependency, or the capacity changes. | You know the limit, for example the size of a connection pool. |

The [bulkhead](../bulkhead/README.md) document describes the fixed policy.

## 6. Interaction with the other policies

The [policy composition](../policy-composition.md) document gives the recommended order.
The limiter is at position 3: inside the rate limiter, and outside the circuit breaker.

```go
pipeline := faultvane.Combine(shedder, rateLimiter, limiter, breaker, retrier, timeout)
```

| Policy | Interaction |
|---|---|
| Load shedding, rate limiter | They are outside the limiter. A call that they reject does not reach the limiter. It is not a call in progress and gives no signal. |
| Circuit breaker | It is inside the limiter. A rejection by the limiter does not reach the breaker. The breaker does not count it as a failure. A rejection by the breaker is a normal result for the limiter. It is not an overload signal by default. |
| Retry | It is inside the limiter. The limiter sees one result for each request: the result of the last attempt. The measured latency contains all attempts and the waits between them. Set `LatencyThreshold` for the full request. A retry policy outside the limiter must not try again after `ErrLimitExceeded`, because it matches `faultvane.ErrRejected`. |
| Timeout | It is inside the limiter. When the timeout of the last attempt ends, the call returns an error that matches `context.DeadlineExceeded`. This is the default overload signal. |
| Context deadline | When the deadline of the request ends during the call, the call usually returns `context.DeadlineExceeded`. This is also an overload signal by default. |

## 7. Design

The limiter has two atomic values for the decision, three atomic counters for `Stats`, and no lock.

| Value | Content | Change |
|---|---|---|
| Calls in progress | An integer | A compare-and-swap operation at the start of a call. An atomic subtraction at the end. |
| Limit | A number with a fraction | A compare-and-swap operation after a call that changes the limit. |

The decision reads the two values and makes one compare-and-swap operation.
The operation succeeds only if the number of calls in progress did not change after the read.
If it changed, the limiter makes the decision again.
For this reason, concurrent callers never get more calls in progress than the limit, and the limiter never rejects a call while the number is below the limit.

The adaptation reads the limit, calculates the new limit, and writes it with a compare-and-swap operation.
If a different call changed the limit in that time, the limiter calculates again.
No change is lost.

The limiter reads the clock only when `LatencyThreshold` is set.

### Measurements for this design

| Alternative | Result | Decision |
|---|---|---|
| Read the clock for each call | 67 ns for each call. Without the clock: 8 ns. | The limiter reads the clock only when `LatencyThreshold` is set. |
| Add 1 to the calls in progress, then compare, then subtract 1 for a rejection | 80 ns in the parallel benchmark. Compare-and-swap: 225 ns. | Not used. During many rejections, the rejected callers increase the count for a short time. The limiter then rejects calls below the limit, and the busy rule sees a number that is too high. A correct decision is more important than the time in a benchmark with an empty call. |
| A mutex for the limit | Not measured. | Not used. The atomic value is less code and has no lock. |

Examine the second decision again if a profile of a real service shows the limiter as a bottleneck.

## 8. Performance

Measured with `go test -run '^$' -bench . -benchmem -count=3 ./concurrencylimit`.
Machine: Apple M4, 10 cores, macOS. Go 1.27.3. Each value is the middle result of three runs.

| Benchmark | Time for each call | Allocations |
|---|---|---|
| `BenchmarkDo`: default options, one goroutine | 8.0 ns | 0 |
| `BenchmarkDoLatencyThreshold`: with the system clock | 67.6 ns | 0 |
| `BenchmarkDoAdapting`: each call changes the limit | 10.8 ns | 0 |
| `BenchmarkDoParallel`: 10 goroutines in parallel, empty call | 225 ns | 0 |
| `BenchmarkDoRejected`: rejection | 2.2 ns | 0 |

The parallel benchmark runs an empty call, so all goroutines change the same counter all the time.
This is the worst condition.
A real call to a dependency takes much longer than the limiter.

## 9. Limits

- The limit is local to one process. Instances of a service do not share it.
- Each overload signal decreases the limit. When many calls in progress fail at the same time, the limit decreases one time for each of them. It can go to `MinLimit` quickly. Set `MinLimit` to the smallest number of concurrent calls that you always permit.
- The limit increases only under load. After an overload, a service with little traffic keeps a low limit until the traffic increases.
- `LatencyThreshold` is a fixed value. The limiter does not learn the normal latency of the dependency.
- The limiter has no queue. A caller does not wait for a free place.
- A panic of the call gives no signal.
- The limiter writes no logs and has no observer. Use `Stats` and the metrics.
