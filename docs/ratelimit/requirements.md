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

Without a store, the rate limiter works in one process. Each limiter keeps its own bucket in local memory.
With a store, the limiters of many processes share one bucket. Sections 8 to 12 give the requirements for this function.

## 3. Terms

| Term | Meaning |
|---|---|
| Token | The permission for one call. |
| Bucket | The store of the tokens of one limiter. |
| Rate | The number of tokens that the bucket gets each second. |
| Burst | The largest number of tokens that the bucket can hold. |
| Store | The system that keeps the shared bucket, for example Redis. |
| Shared bucket | The bucket in the store. All limiters with one name use it. |
| Allowance | The tokens that one limiter holds in local memory. The limiter got them from the shared bucket. |
| Synchronization | One exchange of a limiter with the store. The limiter gets tokens or gives tokens back. |
| Batch | The number of tokens that a limiter wants to hold after a synchronization. |
| Distributed mode | The normal mode of a limiter with a store. The limiter synchronizes with the store. |
| Local mode | The degraded mode. The limiter cannot reach the store and uses a local bucket. |

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
| RL-18 | The limiter starts no goroutine. RL-31 gives the exception for a limiter with a store. |

## 7. Performance

| ID | Requirement |
|---|---|
| RL-19 | A permitted call and a rejected call make no memory allocation in the limiter. |

## 8. Shared limit

| ID | Requirement |
|---|---|
| RL-20 | With the option `WithStore`, all limiters that use one store and one name share one bucket. A limiter without a store works as sections 4 to 7 define. |
| RL-21 | The limiter uses the store only through the interface `Store`. The interface has one operation, `Acquire`. The in-memory store `NewMemoryStore` implements it. Each store passes the conformance suite in `ratelimit/storetest`. [Store](store.md) gives the contract. |
| RL-22 | A call takes one token from the allowance in local memory. A call never calls the store. When the allowance is empty, the limiter rejects the call. |
| RL-23 | A synchronizer in the background makes one store operation in each synchronization interval. The number of store operations does not depend on the number of calls. |
| RL-24 | In each synchronization, the batch is two times the number of calls of the last interval. Permitted calls and rejected calls count. The batch is at least 1 and at most the burst. The limiter asks the store for the tokens that it needs to hold the batch. It gives back the tokens that it holds above the batch. |
| RL-25 | `New` makes the first synchronization. The first batch is the rate multiplied by the synchronization interval, rounded up, and at most the burst. |
| RL-26 | In each time window of `t` seconds, the limiters of one name in distributed mode permit together at most `burst + rate × t + H` calls. `H` is the number of tokens that the limiters hold at the start of the window. One limiter never holds more than its batch. For a window that starts before the first limiter starts, `H` is 0. |

## 9. Store faults

| ID | Requirement |
|---|---|
| RL-27 | After `FaultThreshold` failed synchronizations in sequence, the limiter changes to local mode. In local mode it uses a local bucket with the rate `LocalRate`. The local burst is the burst multiplied by `LocalRate` and divided by the rate, rounded up. The limiter keeps the tokens that it holds, up to the local burst. |
| RL-28 | A fault of the store never fails a call and never blocks a call. A store operation ends at the time `StoreTimeout`. The tokens of a failed synchronization are lost. They never increase the number of permitted calls. |
| RL-29 | In local mode, the limiter makes one store operation in each synchronization interval. The first operation that succeeds returns the limiter to distributed mode. The allowance is then the grant of the store. The limiter does not give tokens of the local bucket to the store. |
| RL-30 | When the first synchronization fails, the limiter starts in local mode with a full local bucket. |
| RL-31 | A limiter with a store starts one goroutine. `Close` stops it. After `Close`, the limiter is in local mode. `Close` of a limiter without a store does nothing. |

## 10. Configuration of the shared limit

| ID | Requirement |
|---|---|
| RL-32 | The option `WithStore` sets the store. The value must not be nil. A limiter with a store must have a name from the option `WithName`. The name has 1 to 64 characters from `a-z`, `A-Z`, `0-9`, `.`, `_`, and `-`. |
| RL-33 | The option `WithSyncInterval` sets the synchronization interval. The default is 100 ms. The option `WithStoreTimeout` sets the time limit of a store operation. The default is 50 ms. The value must be above 0. The synchronization interval must be longer than the store timeout. |
| RL-34 | The option `WithLocalRate` sets the rate of local mode. The default is the rate. The value must be above 0 and at most the rate. The option `WithFaultThreshold` sets the number of failed synchronizations before local mode. The default is 3. The value must be 1 or more. |

## 11. Statistics

| ID | Requirement |
|---|---|
| RL-35 | The method `Stats` returns the number of permitted calls, the number of rejected calls, the mode, the number of failed synchronizations, and the number of whole tokens that the limiter holds now. A limiter without a store is in local mode. |
| RL-36 | The function `RegisterRateLimitMetrics` of the module `otel` gives these statistics as OpenTelemetry metrics. The metric names start with `faultvane.rate_limiter.`. Each attribute has a fixed set of values, except the policy name that the application gives. |

## 12. Redis store

| ID | Requirement |
|---|---|
| RL-37 | The Redis store keeps the shared bucket of one name in one hash with the key `<prefix>:{<namespace>:<name>}:rl:bucket`. One script makes each operation atomic and uses the time of Redis. Each operation sets the time to live of the key. |
| RL-38 | A fault of Redis and an unexpected reply return an error that matches `faultvane.ErrStoreUnavailable`. The error starts with `redisstore: rate limit acquire:`. The store sends the script again when Redis lost its script cache. |
| RL-39 | Acceptance: 10 limiters under heavy load share one rate through Redis. The permitted calls stay inside the bound of RL-26 and are at least 70% of the rate multiplied by the time. |
| RL-40 | Acceptance: when Redis is not available, the limiters change to local mode and permit calls at the local rate. When Redis is available again, the limiters return to distributed mode. |

## 13. Performance of the shared limit

| ID | Requirement |
|---|---|
| RL-41 | A permitted call and a rejected call of a limiter with a store make no memory allocation in the limiter. |

## 14. Events and logs

| ID | Requirement |
|---|---|
| RL-42 | The option `WithObserver` sets the observer of the events. The default is no observer. The option `WithLogger` sets the logger of the package `log/slog`. The default is `slog.Default()`. The values must not be nil. A limiter without a store ignores these options and gives no event and no log. |
| RL-43 | The limiter gives a `ModeChanged` event for each mode change. The event contains the name of the limiter, the mode after the change, and the store error when a store fault is the cause. A limiter that starts in local mode because the first synchronization failed gives this event. A limiter that starts in distributed mode gives no event. `Close` of a limiter in distributed mode gives this event without an error. |
| RL-44 | The limiter gives a `StoreFault` event with the store error for each failed synchronization in distributed mode and for a failed first synchronization. A failed synchronization in local mode gives no event and no log. It only increases the number of failed synchronizations in `Stats`. |
| RL-45 | The limiter writes one log record for each event before it calls the observer. A change to local mode after a store fault has the level `WARN`. A different mode change has the level `INFO`. A store fault has the level `DEBUG`. A record contains only the name of the limiter, the mode, and the text of the store error. |
| RL-46 | The limiter calls the observer outside its lock, in the goroutine of the synchronizer or of the caller of `New` or `Close`. An observer that does not return blocks no call. When the observer panics, the limiter recovers, writes a log record with the level `ERROR`, and continues. |
| RL-47 | A call gives no event and no log record. A permitted call and a rejected call make no memory allocation in a limiter with an observer and a logger. |

## 15. Out of scope

- A call that waits for a token.
- A call that needs more than one token.
- A separate limit for each client or each key. The application can keep one limiter for each key.
