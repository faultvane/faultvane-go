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
| State check before each attempt | When a retry policy runs inside a breaker, it reads the breaker state before each new attempt. If the circuit is not `CLOSED`, it stops. This is a read of memory only. |
| Retry budget | The retry policy permits retries only up to a configured part of the normal calls, for example 10%. During an outage, the extra traffic has this limit. |

### Probes do not retry

A call that holds a probe lease makes one attempt only.
A probe must give a clear result quickly.

## 3. Results of other policies

| Event | Result for the breaker |
|---|---|
| Rejection by load shedding, rate limiter, or bulkhead | Not counted. The call does not reach the breaker. |
| Timeout of an attempt, and the last attempt | Failure |
| The caller cancels the context | Ignored |
| The deadline of the request ends | Failure |

## 4. Failure chain that the composition prevents

```text
Dependency failure
  → retry storm            prevented by: retry budget, state check
  → goroutine exhaustion   prevented by: timeout, bulkhead
  → application failure    prevented by: load shedding
  → cascading failure      prevented by: circuit breaker
```

## 5. Status

The circuit breaker is the first component.
Timeout and retry are the next components, because the breaker rules above need them.
The [roadmap](roadmap.md) gives the order.
