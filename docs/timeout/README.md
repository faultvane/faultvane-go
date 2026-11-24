# Timeout

The timeout policy gives each call a time limit.
It protects the application from a dependency that became slow.

Status: implemented.

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the timeout must do. Each requirement has an identifier. |

## 1. API

```go
import "github.com/faultvane/faultvane-go/timeout"
```

| Item | Signature | Purpose |
|---|---|---|
| `New` | `func New(d time.Duration) (*Timeout, error)` | Create a timeout with the time limit `d`. |
| `Do` | `func (t *Timeout) Do(ctx context.Context, call func(context.Context) error) error` | Run the call with the time limit. |
| `ErrTimeout` | `var ErrTimeout error` | The error that shows that the time limit ended. |

`*Timeout` implements `faultvane.Policy`.

### Example: a call with a time limit

```go
limit, err := timeout.New(2 * time.Second)
if err != nil {
    return err
}

err = limit.Do(ctx, func(ctx context.Context) error {
    request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return err
    }
    response, err := client.Do(request)
    if err != nil {
        return err
    }
    return response.Body.Close()
})
if errors.Is(err, timeout.ErrTimeout) {
    return fmt.Errorf("the call did not complete in 2 seconds: %w", err)
}
```

### Example: a call that returns a value

`faultvane.Execute` works with each policy.

```go
user, err := faultvane.Execute(ctx, limit, func(ctx context.Context) (User, error) {
    return repository.Find(ctx, id)
})
```

## 2. Configuration

The timeout has one value.

| Value | Default | Validation rule |
|---|---|---|
| Time limit `d` | None. The application must give it. | Must be above 0. |

When the time limit is not valid, `New` returns an error that matches `faultvane.ErrInvalidConfig`.
The message is `faultvane: invalid configuration: duration must be above 0`.

## 3. Behavior

`Do` makes a context for the call from the context of the caller.
The context of the call ends when the time limit ends.
`Do` runs the call one time, in the goroutine of the caller.
`Do` returns when the call returns.

### The call must obey its context

The policy does not stop the call.
It ends the context of the call. The call must then return.
A call that does not read its context runs to its end, and `Do` waits for it.

Give the context of the call to each operation that can block: a network request, a database query, a wait on a channel.

The policy runs the call in the goroutine of the caller for these reasons:

- No goroutine continues after `Do` returns. A goroutine leak is not possible.
- The call never runs at the same time as the code after `Do`. No data race is possible between them.
- A panic of the call continues to the caller with the same value.

### Result rules

| Result of the call | Condition | Result of `Do` |
|---|---|---|
| nil | Each condition | nil |
| Error | The time limit of the policy did not end. | The error of the call, without a change |
| Error | The context of the caller ended. | The error of the call, without a change |
| Error | The time limit of the policy ended, and the context of the caller did not end. | An error that matches `ErrTimeout` and the error of the call |

A call that completes with nil after the time limit is a success.
The policy does not discard a result that the dependency gave.

### The timeout error

The timeout error contains the error of the call.

| Expression | Result |
|---|---|
| `errors.Is(err, timeout.ErrTimeout)` | `true` |
| `errors.Is(err, context.DeadlineExceeded)` | `true` when the call returned the error of its context |
| `errors.As(err, &target)` | `true` when the error of the call has the type of `target` |
| `errors.Is(err, faultvane.ErrRejected)` | `false` |

`ErrTimeout` is not a rejection, because the call ran.
The dependency possibly received the request and completed the work.
Try the call again only when the operation is safe to repeat.

### Context of the caller

- The context of the call has the values of the context of the caller.
- A cancellation by the caller ends the context of the call immediately.
- The time limit never extends the deadline of the caller. The earlier deadline applies.
- When the context of the caller ended, the result is not a timeout of the policy. `Do` returns the error of the call without a change.

### Panic

A panic of the call continues to the caller with the same value.
The policy releases the context of the call first.

## 4. Interaction with the other policies

[Policy composition](../policy-composition.md) gives the recommended order.
The timeout is the innermost policy.

```go
pipeline := faultvane.Combine(breaker, retrier, limit)
```

| Policy | Interaction |
|---|---|
| Retry | With the timeout inside the retry, each attempt gets the full time limit. `ErrTimeout` is not a rejection, so the default retry rule tries the call again. |
| Retry, different order | With the timeout outside the retry, the time limit applies to all attempts and all waits together. When the time limit ends, the retry stops. |
| Circuit breaker | The default classifier of the breaker counts an `ErrTimeout` result as a failure. A cancellation by the caller is not a timeout, and the breaker ignores it. |
| Deadline of the request | The total time of a request is limited by the deadline of the context of the caller. The timeout limits one attempt only. |

## 5. Performance

The policy has no state that changes and uses no lock.
A call makes 4 allocations. They are for the context of the call and its timer.
For this reason the timeout does not satisfy the zero allocation rule TK-50. A test asserts the limit of 4 allocations.

Measured on an Apple M4 with 10 cores, Go 1.27.3, one run:

| Benchmark | Time for each call | Allocations |
|---|---|---|
| `Do`, one goroutine | 171 ns | 4 |
| `Do`, the caller has a deadline, one goroutine | 211 ns | 4 |
| `Do`, 10 goroutines in parallel | 95 ns | 4 |

Run the benchmarks:

```sh
go test -run '^$' -bench . -benchmem ./timeout
```

## 6. Limits

- The policy cannot stop a call that does not obey its context.
- The policy does not know if the dependency completed the work before the time limit ended.
- When an error of the call and the end of the time limit occur at almost the same time, the result can be a timeout error. It contains the error of the call.
- The time limit is fixed for the life of a policy instance. For a different time limit, create a second instance.
- The policy has no metrics and no logs yet. Observability is planned for all policies.
