# Rate limiter

The rate limiter limits the number of calls that start in a time period.
It works in one process and keeps its state in local memory.

The [requirements](requirements.md) list what the limiter must do. Each requirement has an identifier.

## 1. Purpose

Use the rate limiter in these conditions:

- A dependency accepts only a fixed number of calls each second.
- A service must limit the calls that it accepts each second.

The limiter uses the token bucket algorithm.
The bucket holds tokens. A call needs one token.
The bucket gets new tokens at a constant rate, up to a maximum that is the burst.
When the bucket holds no token, the limiter rejects the call. The limiter does not wait.

## 2. API

The package is `github.com/faultvane/faultvane-go/ratelimit`.

| Item | Function |
|---|---|
| `New(opts ...Option) (*Limiter, error)` | Makes a limiter. It returns an error when the configuration is invalid. |
| `(*Limiter).Do(ctx, call) error` | Runs the call when a token is available. A `Limiter` is a `faultvane.Policy`. |
| `(*Limiter).Allow() bool` | Removes one token and returns `true`. It returns `false` when no token is available. |
| `ErrRateLimited` | The error of a rejected call. It matches `faultvane.ErrRejected`. |

### Limit a function

```go
limiter, err := ratelimit.New(
    ratelimit.WithRate(50),
    ratelimit.WithBurst(10),
)
if err != nil {
    return err
}

err = limiter.Do(ctx, func(ctx context.Context) error {
    return client.Send(ctx, message)
})
if errors.Is(err, ratelimit.ErrRateLimited) {
    return errTryLater
}
```

### Make only the decision

Use `Allow` when the code does not put the work in a function, for example in an HTTP handler.

```go
if !limiter.Allow() {
    http.Error(w, "too many requests", http.StatusTooManyRequests)
    return
}
```

### Return a value

`faultvane.Execute` returns the value of a call.

```go
user, err := faultvane.Execute(ctx, limiter, func(ctx context.Context) (User, error) {
    return client.User(ctx, id)
})
```

## 3. Options

| Option | Default | Valid values | Function |
|---|---|---|---|
| `WithRate(perSecond float64)` | 100 | Above 0 and at most 1e9 | The number of tokens that the bucket gets each second. |
| `WithBurst(n int)` | The rate rounded up to a whole number | 1 or more | The largest number of tokens that the bucket can hold. |
| `WithClock(clock faultvane.Clock)` | `faultvane.SystemClock()` | Not nil | The source of time. Tests use `faultvanetest.Clock`. |

An invalid value makes `New` return an error that matches `faultvane.ErrInvalidConfig`.
The error message names the option and the rule, for example `faultvane: invalid configuration: Rate must be above 0 and at most 1e9`.

A rate below 1 is valid. For example, a rate of 0.2 gives one token each 5 seconds.

When you set only the rate, the burst follows the rate. A rate of 2.5 gives a burst of 3.

## 4. Behavior rules

1. A new limiter has a full bucket. The first calls, up to the burst, are permitted immediately.
2. A permitted call removes one token.
3. A rejected call removes no token.
4. The bucket gets tokens in proportion to the elapsed time. The limiter calculates the refill when a call arrives. It starts no goroutine and no timer.
5. The bucket never holds more tokens than the burst.
6. A part of a token stays in the bucket. With a rate of 10, one token is available 100 ms after the bucket is empty, and not before that time.
7. The refill has no drift. The limiter calculates the tokens from the time when the bucket was last full. It does not add small amounts for each call.
8. `Do` returns the error of the call without a change.
9. When the context has ended before the call starts, `Do` returns the error of the context. It runs nothing and removes no token.
10. A panic of the call continues to the caller with the same value.
11. All methods are safe for concurrent use.

### Rate and burst

The rate limits the calls over a long time. The burst limits the calls at one instant.
In a period of `t` seconds, the limiter permits at most `burst + rate × t` calls.

| Aim | Configuration |
|---|---|
| Permit short peaks | A burst above the rate |
| Give calls an equal distance in time | A burst of 1 |

## 5. Interaction with the other policies

[Policy composition](../policy-composition.md) gives the recommended order of the policies.
The rate limiter is the second policy from the outside. Load shedding is before it.

```go
pipeline := faultvane.Combine(shedder, limiter, breaker)
```

| Policy | Interaction |
|---|---|
| Load shedding | It is outside the rate limiter. Work that load shedding rejects uses no token. |
| Circuit breaker | It is inside the rate limiter. A rejection of the limiter does not reach the breaker, so the breaker does not count it as a failure. |
| Retry | A retry policy must not try again after `faultvane.ErrRejected`. With the limiter outside the retry policy, one request uses one token. With the limiter inside the retry policy, each attempt uses one token. |

A token that a call removed does not come back. This is also true when a policy inside the limiter rejects the call.

## 6. Limits

- The limit applies to one process. Each instance of a service has its own bucket. With 10 instances and a rate of 100, the dependency can receive 1000 calls each second. A limit that many processes share is a later evaluation.
- The limiter does not wait for a token. The application decides what to do after a rejection.
- A call needs exactly one token. A weight for each call is not available.
- One limiter has one bucket. For a limit for each client, the application keeps one limiter for each client and sets a limit on the number of limiters.
- The limiter calculates with 64-bit floating-point numbers. An error of calculation is possible only when the bucket does not become full for more than approximately 10^15 tokens. That error is far below one token in normal use.
- The limiter protects its bucket with a mutex. The benchmarks in `ratelimit/bench_test.go` measure the cost. A lock-free design was slower with 10 parallel goroutines on the test machine.
