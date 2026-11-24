# Policy composition

A circuit breaker is one of many resilience policies.
This document defines the recommended order of the policies and the interaction between the breaker and retries.

## 1. Recommended order

The first policy in this list is the outermost policy.

```text
Request
  → Load shedding
    → Rate limiter
      → Bulkhead or concurrency limiter
        → Circuit breaker
          → Retry
            → Timeout (for each attempt)
              → Dependency
```

| Position | Policy | Reason for the position |
|---|---|---|
| 1 | Load shedding | It protects the application itself. It must reject work before the work uses resources. |
| 2 | Rate limiter | It rejects calls above the permitted rate at low cost. |
| 3 | Bulkhead or concurrency limiter | It limits the resources that one dependency can use. Slow calls then cannot use all goroutines or connections. |
| 4 | Circuit breaker | It sees one result for each application request. Rejections of the outer policies are not failures of the dependency. |
| 5 | Retry | It runs inside the breaker. The breaker counts the final result. |
| 6 | Timeout | It limits each attempt. A timeout is a failure that the retry policy can try again. |

The total time of a request is limited by the deadline of its context.

## 2. Breaker and retries

### One request is one call

With the recommended order, the breaker counts one application request as one call.
A request with three attempts that all fail is one failure.
A request that succeeds at the third attempt is one success.

To count each attempt, put the breaker inside the retry policy.
This is a decision of the application.

### Retries must not multiply traffic

During an outage, three attempts for each request give three times the traffic.
The design has three protections:

| Protection | Behavior |
|---|---|
| Rejections are final | The retry policy never tries again after `faultvane.ErrRejected`. |
| State check before each attempt | The option `retry.WithGate` gives the retry policy a function. The retry policy calls the function before each new attempt. When the function returns `false`, the retry policy stops and returns the last error. The application sets this option. Without it, the retry policy makes no state check. |
| Retry budget | The retry policy permits retries only up to a configured part of the normal calls, for example 10%. During an outage, the extra traffic has this limit. |

The retry policy does not import the circuit breaker package.
The application connects the two with a function that reads the breaker state.
`State` reads memory only.

```go
retrier, err := retry.New(
    retry.WithMaxAttempts(3),
    retry.WithBudget(0.1),
    retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
)
```

### Probes do not retry

A probe must give a clear result quickly.
With the gate above, a call that holds a probe lease makes one attempt only, because the circuit is `HALF_OPEN` during a probe.
Without the gate, the retry policy can run a probe call again.

## 3. Results of other policies

| Event | Result for the breaker |
|---|---|
| Rejection by load shedding, rate limiter, or bulkhead | Not counted. The call does not reach the breaker. |
| Timeout of the last attempt | Failure |
| The call returns an error that matches `context.Canceled` | Ignored |
| The call returns `context.DeadlineExceeded` | Failure |

These results come from the default classifier of the breaker.
The application can replace it with `circuitbreaker.WithClassifier`.

## 4. Failure chain that the composition prevents

```text
Dependency failure
  → retry storm            prevented by: retry budget, state check
  → goroutine exhaustion   prevented by: timeout, bulkhead
  → application failure    prevented by: load shedding
  → cascading failure      prevented by: circuit breaker
```

## 5. Pipeline

The application builds the order with `faultvane.Combine`.
The first argument is the outermost policy.

```go
breaker, err := circuitbreaker.New("payment-service")
if err != nil {
    return err
}
shedder, err := loadshed.New(loadshed.WithMaxInFlight(200))
if err != nil {
    return err
}
limiter, err := ratelimit.New(ratelimit.WithRate(100), ratelimit.WithBurst(20))
if err != nil {
    return err
}
slots, err := bulkhead.New(bulkhead.WithMaxConcurrent(50))
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

pipeline := faultvane.Combine(shedder, limiter, slots, breaker, retrier, attempt)
```

The library does not change the order that the application gives.
The program in [`examples/pipeline`](../examples/pipeline/main.go) runs a pipeline of a bulkhead, a breaker, a retry, and a timeout. A test checks its output.

## 6. Status

All policies of this document are implemented.
The test `TestPolicyPipeline` in the core module runs the policies together in the recommended order.
The state check is an option of the application. The library does not add it automatically.
