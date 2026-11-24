# Bulkhead

The bulkhead limits the number of calls that run at the same time for one dependency.
One slow dependency then cannot use all goroutines, memory, and connections of the service.

Status: implemented in the package `bulkhead`. Metrics through the observer are planned.

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the bulkhead must do. Each requirement has an identifier. |

## 1. Purpose

Each bulkhead has a fixed number of slots.
A call holds one slot while it runs.
When no slot is free, the bulkhead rejects the call, or the caller waits for a limited time.

Use one bulkhead for each dependency.
The limit of one bulkhead does not change the limit of a different bulkhead.

## 2. API

```go
func New(opts ...Option) (*Bulkhead, error)

func (b *Bulkhead) Do(ctx context.Context, call func(context.Context) error) error
func (b *Bulkhead) InFlight() int
func (b *Bulkhead) Waiting() int

var ErrFull error
```

| Item | Behavior |
|---|---|
| `New` | Creates a bulkhead. It returns an error that matches `faultvane.ErrInvalidConfig` when an option is not valid. |
| `Do` | Gets a slot, runs the call, and frees the slot. It returns the error of the call without a change. |
| `InFlight` | Returns the number of calls in progress. |
| `Waiting` | Returns the number of waiting callers. |
| `ErrFull` | The rejection error. It matches `faultvane.ErrRejected`. |

`*Bulkhead` implements `faultvane.Policy`.
All methods are safe for concurrent use.

### Example

```go
payments, err := bulkhead.New(
    bulkhead.WithMaxConcurrent(20),
    bulkhead.WithMaxWaiting(10),
    bulkhead.WithMaxWait(50*time.Millisecond),
)
if err != nil {
    panic(err)
}

err = payments.Do(context.Background(), chargeCard)
switch {
case errors.Is(err, bulkhead.ErrFull):
    fmt.Println("the payment service has no free slot")
case errors.Is(err, faultvane.ErrRejected):
    fmt.Println("a policy rejected the call")
case err != nil:
    fmt.Println("the call failed:", err)
}
```

To get a value from the call, use `faultvane.Execute`:

```go
receipt, err := faultvane.Execute(ctx, payments, func(ctx context.Context) (Receipt, error) {
    return client.Charge(ctx, order)
})
```

### Status for metrics

```go
fmt.Println(payments.InFlight(), payments.Waiting())
```

Read these values at an interval and export them as gauges.

## 3. Options

| Option | Default | Valid values | Meaning |
|---|---|---|---|
| `WithMaxConcurrent(n)` | `10` | 1 or more | The number of slots. This is the maximum number of calls in progress. |
| `WithMaxWaiting(n)` | `0` | 0 or more | The number of waiting places. With `0`, no caller waits. |
| `WithMaxWait(d)` | `0` | 0 or more | The maximum time that a caller waits. With `0`, the caller waits until its context ends. |

`MaxWait` has an effect only when `MaxWaiting` is above 0.
When `MaxWaiting` is above 0 and `MaxWait` is 0, give each call a context with a deadline.
Without a deadline, a caller can wait for an unlimited time.

## 4. Behavior rules

`Do` applies these rules in this order:

1. If the context has already ended, `Do` returns the error of the context. It runs nothing.
2. If a slot is free, the call runs.
3. If no waiting place is free, `Do` returns `ErrFull` immediately.
4. If a waiting place is free, the caller waits.

A waiting caller stops waiting for one of three reasons:

| Event | Result |
|---|---|
| A call in progress returns, and this caller is the first in the queue. | The caller gets the slot. Its call runs. |
| The context of the caller ends. | `Do` returns the error of the context. The call does not run. |
| `MaxWait` ends. | `Do` returns `ErrFull`. The call does not run. |

More rules:

- The queue has the order of arrival. The caller that waits longest gets the next free slot.
- A free slot goes directly to the first waiting caller. A new caller cannot take it first.
- When a caller gets a slot at the same moment that its context or its `MaxWait` ends, its call runs. The call receives the context and can stop immediately.
- When the call returns an error, the slot becomes free. The error returns without a change.
- When the call panics, the slot becomes free. The panic continues with the same value.
- The bulkhead starts no goroutine and has no `Close` method.
- The bulkhead does not stop a call in progress. Use a timeout for that.

## 5. Bulkhead or concurrency limiter

Both policies limit the calls in progress for one dependency.
Use one of them for one dependency, not both.

| Subject | Bulkhead | Concurrency limiter |
|---|---|---|
| Limit | Fixed. The application sets it. | Adaptive. The limiter changes it between a minimum and a maximum. |
| Input of the decision | The number of calls in progress. | The number of calls in progress, and the results of the last calls. |
| Waiting | Optional, with a limit of callers and of time. | None. |
| Purpose | Protect the resources of the service from one slow dependency. | Find the number of concurrent calls that the dependency can serve. |
| Use it when | You know the limit, for example the size of a connection pool. | You do not know the capacity of the dependency, or the capacity changes. |

The [concurrency limiter](../concurrencylimit/README.md) document describes the adaptive policy.

## 6. Interaction with the other policies

The [policy composition](../policy-composition.md) document gives the recommended order.
The bulkhead is at position 3: inside the rate limiter, and outside the circuit breaker.

```go
pipeline := faultvane.Combine(shedder, rateLimiter, payments, breaker, retrier, timeout)
```

| Policy | Interaction |
|---|---|
| Load shedding, rate limiter | They are outside the bulkhead. A call that they reject uses no slot. |
| Circuit breaker | It is inside the bulkhead. A rejection by the bulkhead does not reach the breaker. The breaker does not count it as a failure. When the circuit is open, the breaker rejects quickly, and the slot becomes free immediately. |
| Retry | It is inside the bulkhead. One request holds one slot for all its attempts and the waits between them. A retry policy outside the bulkhead must not try again after `ErrFull`, because `ErrFull` matches `faultvane.ErrRejected`. |
| Timeout | It is inside the bulkhead. It limits the time of each attempt, and thus the time that a slot is in use. Without a timeout, a dependency that does not answer holds its slots for an unlimited time. |
| Context deadline | The time that a caller waits for a slot is part of the time of the request. The deadline of the context limits the wait and the call together. |

## 7. Design

The bulkhead has one mutex.
The mutex protects the number of calls in progress and the queue of waiting callers.
The bulkhead holds the mutex only for a few instructions, never during the call and never during the wait.

A call that finds a free slot locks the mutex two times: one time to get the slot and one time to free it.
It makes no memory allocation.
A caller that waits makes a small number of allocations for its place in the queue and for the `MaxWait` timer.

## 8. Performance

Measured with `go test -run '^$' -bench . -benchmem -count=3 ./bulkhead`.
Machine: Apple M4, 10 cores, macOS. Go 1.27.3. Each value is the middle result of three runs.

| Benchmark | Time for each call | Allocations |
|---|---|---|
| `BenchmarkDo`: free slot, one goroutine | 9.5 ns | 0 |
| `BenchmarkDoParallel`: free slot, 10 goroutines in parallel, empty call | 183 ns | 0 |
| `BenchmarkDoFull`: rejection | 4.5 ns | 0 |

The parallel benchmark runs an empty call, so all goroutines compete for the mutex all the time.
This is the worst condition.
A real call to a dependency takes much longer than the bulkhead.

## 9. Limits

- The limit is local to one process. Instances of a service do not share slots.
- The limit is fixed. The bulkhead does not adapt it to the health of the dependency.
- `MaxWait` uses the real time of the system. The bulkhead has no clock option.
- A waiting caller blocks its goroutine. Keep `MaxWaiting` small.
- The bulkhead does not report events to an observer yet. Use `InFlight` and `Waiting` for metrics.
- The bulkhead has no priority classes. All callers are equal.
