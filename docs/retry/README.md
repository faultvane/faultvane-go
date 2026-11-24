# Retry

The retry policy runs a call again when the call fails.
It helps with failures that are short.
A gate and a budget prevent retries from multiplying the traffic during an outage.

Status: implemented.

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the retry must do. Each requirement has an identifier. |

## 1. API

```go
import "github.com/faultvane/faultvane-go/retry"
```

| Item | Signature | Purpose |
|---|---|---|
| `New` | `func New(opts ...Option) (*Retry, error)` | Create a retry policy. |
| `Do` | `func (r *Retry) Do(ctx context.Context, call func(context.Context) error) error` | Run the call, and run it again after an error. |
| `Option` | `type Option func(*config)` | One configuration value. |

`*Retry` implements `faultvane.Policy`.
The policy has no error of its own. `Do` returns nil or the error of the last attempt.

### Example: default configuration

```go
retrier, err := retry.New()
if err != nil {
    return err
}

err = retrier.Do(ctx, func(ctx context.Context) error {
    return client.Send(ctx, message)
})
```

### Example: custom retry rule and budget

```go
retrier, err := retry.New(
    retry.WithMaxAttempts(4),
    retry.WithBackoff(50*time.Millisecond, 2*time.Second),
    retry.WithRetryIf(func(err error) bool {
        return !errors.Is(err, ErrNotFound)
    }),
    retry.WithBudget(0.1),
)
```

### Example: a call that returns a value

`faultvane.Execute` works with each policy.

```go
user, err := faultvane.Execute(ctx, retrier, func(ctx context.Context) (User, error) {
    return repository.Find(ctx, id)
})
```

## 2. Options

| Option | Default | Meaning | Validation rule |
|---|---|---|---|
| `WithMaxAttempts(attempts)` | `3` | The maximum number of attempts. The number includes the first attempt. With `1`, the policy makes no retry. | Must be 1 or more. |
| `WithBackoff(initial, maximum)` | `100ms`, `10s` | The ceiling of the wait before the first retry, and the largest ceiling. | `initial` must be above 0. `maximum` must not be below `initial`. |
| `WithRetryIf(retryIf)` | Each error is tried again. | The custom retry rule. The function returns `true` when the policy can try the error again. | Must not be nil. |
| `WithGate(gate)` | Always open | A function that the policy calls before each retry. When it returns `false`, the policy stops. | Must not be nil. |
| `WithBudget(ratio)` | Off | The retry budget. `ratio` is the number of tokens that each first attempt adds. | Must be above 0 and at most 1. |

When an option is not valid, `New` returns an error that matches `faultvane.ErrInvalidConfig`.
The message names the option, for example `faultvane: invalid configuration: MaxAttempts must be 1 or more`.

The functions of `WithRetryIf` and `WithGate` run in the request path.
They must be fast, must not block, and must be safe for concurrent use.

## 3. Behavior

### Sequence

`Do` always runs the first attempt.
After each attempt, the policy examines these conditions in this order.
At the first condition that is true, `Do` returns the result of the attempt.

| Order | Condition | Result of `Do` |
|---|---|---|
| 1 | The attempt returned nil. | nil |
| 2 | The attempt was attempt number `MaxAttempts`. | The error of the attempt |
| 3 | The error is a final error. | The error of the attempt |
| 4 | The gate returns `false`. | The error of the attempt |
| 5 | The budget is on and has no full token. | The error of the attempt |
| 6 | The context of the caller ends during the wait. | The error of the attempt |

If no condition is true, the policy makes the next attempt.

The policy examines the gate and the budget before the wait.
The caller thus gets the error immediately when a retry is not permitted.

### Final errors

An error is final in these conditions:

- The error matches `faultvane.ErrRejected`. A policy rejected the call. A second attempt gets the same rejection and adds load. This rule has no option, and the custom retry rule is not asked.
- The context of the caller ended. The caller does not wait for the result.
- The custom retry rule returns `false` for the error.

The default rule tries each other error again.
This includes a context error that the call returns while the context of the caller is active, for example the error of a timeout policy inside the retry.

### Returned error

`Do` returns the error of the last attempt without a change.
It does not wrap the error and it does not collect the errors of earlier attempts.

When the context of the caller ends during a wait, `Do` returns the error of the last attempt, not the error of the context.
The reason: the error of the call tells the application why the operation failed. The application can read the state of the context from the context itself.

### Backoff

The wait before a retry is a random duration from 0 to below a ceiling. This method is exponential backoff with full jitter.

```text
ceiling of retry n = the smaller of: maximum, initial × 2^(n-1)
wait of retry n    = random duration, 0 ≤ wait < ceiling
```

With the defaults, the ceilings are 100 ms, 200 ms, 400 ms, and so on, up to 10 s.
The random wait spreads the retries of many callers in time. Without it, callers that failed together try again together.

The wait runs in the goroutine of the caller.
It stops immediately when the context of the caller ends.

### Gate

The gate connects the retry to a state of the application without an import between policies.
The policy calls the gate one time before each retry, not before the first attempt.
When the gate returns `false`, the policy stops and returns the error of the last attempt.

### Budget

The budget limits the retries in relation to the normal traffic.

- A new policy has 10 tokens.
- Each first attempt adds `ratio` tokens. A successful and a failed first attempt add the same quantity.
- The policy keeps 10 tokens at most.
- Each retry removes one token.
- Without a full token, the policy makes no retry.

With `WithBudget(0.1)`, 10 requests pay for one retry.
During an outage, the retries add at most 10% to the traffic, after the tokens of the start are used.
The 10 tokens let a service with low traffic retry, and they permit a short burst.

The budget keeps tokens with a precision of one billionth of a token. A smaller ratio adds one billionth of a token for each first attempt.

One policy instance has one budget. Use one instance for each dependency.

### Panic

A panic of the call continues to the caller with the same value.
The policy does not try the call again after a panic.

## 4. Interaction with the other policies

[Policy composition](../policy-composition.md) gives the recommended order.
The retry runs inside the circuit breaker and outside the timeout.

```go
pipeline := faultvane.Combine(breaker, retrier, limit)
```

| Policy | Interaction |
|---|---|
| Timeout | With the timeout inside the retry, each attempt has its own time limit. A timeout of an attempt is not a rejection, so the default rule tries it again. |
| Circuit breaker, outside the retry | The breaker counts one result for each request: the final result of the retry. Connect the gate to the breaker state, so that the retry stops when the circuit is not closed. |
| Circuit breaker, inside the retry | The breaker counts each attempt. A rejection by the breaker is a final error, so the retry stops immediately. |
| Bulkhead, rate limiter, load shedding | A rejection by one of these policies inside the retry is a final error. |
| Deadline of the request | The deadline of the context of the caller limits all attempts and all waits together. |

### Connect the gate to a circuit breaker

```go
breaker, err := circuitbreaker.New("payment-service")
if err != nil {
    return err
}
retrier, err := retry.New(
    retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }),
    retry.WithBudget(0.1),
)
if err != nil {
    return err
}
pipeline := faultvane.Combine(breaker, retrier, limit)
```

The gate reads memory only.
The protections of the composition document map to the policy in this way:

| Protection | Implementation |
|---|---|
| Rejections are final | Built in. No option changes it. |
| State check before each attempt | `WithGate` |
| Retry budget | `WithBudget` |

## 5. Performance

A request with a successful first attempt makes no allocation and uses no lock.
The budget is one atomic value.
The wait before a retry uses a timer, and the timer makes allocations.

Measured on an Apple M4 with 10 cores, Go 1.27.3, one run. The benchmarks replace the wait with a function that returns immediately.

| Benchmark | Time for each call | Allocations |
|---|---|---|
| `Do`, success, no budget, one goroutine | 2.4 ns | 0 |
| `Do`, success, with budget, one goroutine | 3.1 ns | 0 |
| `Do`, success, with budget, 10 goroutines in parallel | 0.5 ns | 0 |
| `Do`, three failed attempts, no budget, one goroutine | 33 ns | 0 |
| `Do`, failure with an empty budget, 10 goroutines in parallel | 227 ns | 0 |

Run the benchmarks:

```sh
go test -run '^$' -bench . -benchmem ./retry
```

## 6. Limits

- The policy does not know if an operation is safe to repeat. Use it only for operations that are idempotent, or give a retry rule that makes other errors final.
- The budget is local to one policy instance in one process. Instances of a service do not share it.
- The cap of 10 tokens is fixed. It is not an option.
- When all requests fail and the budget is empty, all goroutines change the same atomic value. The last benchmark shows the cost of this contention.
- The gate is read before the wait. The state of the application can change during the wait.
- The policy does not read a wait time that a dependency gives, for example a `Retry-After` header.
- The policy has no metrics and no logs yet. Observability is planned for all policies.
