# Rate limiter requirements

This document lists what the rate limiter must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

A service sends calls to a dependency, or receives calls from its clients.
Too many calls in a short time overload the receiver.
The rate limiter limits the number of calls that start in a time period.

## 2. Scope

The rate limiter works in one process.
Each limiter keeps its own bucket in local memory.
A limit that many processes share is not in the scope of this document. The project evaluates it later.

## 3. Terms

| Term | Meaning |
|---|---|
| Token | The permission for one call. |
| Bucket | The store of the tokens of one limiter. |
| Rate | The number of tokens that the bucket gets each second. |
| Burst | The largest number of tokens that the bucket can hold. |

## 4. Behavior

| ID | Requirement |
|---|---|
| RL-1 | The limiter implements the policy interface of the toolkit. It limits the calls that start in a time period in one process. |
| RL-2 | A call needs one token. When the bucket holds a token, the limiter removes one token, runs the call one time, and returns the result of the call without a change. |
| RL-3 | When the bucket holds less than one token, the limiter rejects the call immediately. It does not run the call and it does not wait for a token. |
| RL-4 | A rejected call gets the error `ErrRateLimited`. This error matches `faultvane.ErrRejected`. |
| RL-5 | The bucket of a new limiter is full. It holds a number of tokens equal to the burst. |
| RL-6 | The bucket gets tokens in proportion to the elapsed time of the clock. In one second it gets a number of tokens equal to the rate. A part of a token stays in the bucket. |
| RL-7 | The bucket never holds more tokens than the burst. Time that elapses while the bucket is full gives no tokens. |
| RL-8 | The refill has no drift. After the bucket is empty, exactly N tokens are available after N divided by the rate seconds. The number of clock steps and the number of calls in that time do not change this result. |
| RL-9 | The method `Allow` makes the same decision without a function to call. It removes one token and returns `true`, or it returns `false` when the bucket holds less than one token. |
| RL-10 | A rejected call removes no token and does not delay the refill. |

## 5. Configuration

| ID | Requirement |
|---|---|
| RL-11 | The option `WithRate` sets the rate in tokens for each second. The default is 100. The value must be above 0 and at most 1e9. |
| RL-12 | The option `WithBurst` sets the burst. The default is the rate rounded up to a whole number. The value must be 1 or more. |
| RL-13 | The option `WithClock` sets the clock. The default is the system clock. The value must not be nil. |
| RL-14 | An invalid configuration makes `New` return an error that matches `faultvane.ErrInvalidConfig`. The error names the option and the rule. |

## 6. Context, concurrency, and panic

| ID | Requirement |
|---|---|
| RL-15 | When the context has ended before the call starts, `Do` returns the error of the context. It does not run the call and it removes no token. |
| RL-16 | All methods are safe for concurrent use. When many goroutines call the limiter at one clock instant, the number of permitted calls is exactly the number of whole tokens in the bucket. |
| RL-17 | A panic of the call continues to the caller with the same value. The limiter works correctly after the panic. |
| RL-18 | The limiter starts no goroutine. |

## 7. Performance

| ID | Requirement |
|---|---|
| RL-19 | A permitted call and a rejected call make no memory allocation in the limiter. |

## 8. Out of scope

- A limit that many processes share.
- A call that waits for a token.
- A call that needs more than one token.
- A separate limit for each client or each key. The application can keep one limiter for each key.
