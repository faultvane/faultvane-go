# Concurrency limiter requirements

This document lists what the concurrency limiter must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

A fixed limit of concurrent calls is correct only for one capacity of the dependency.
The capacity changes: the dependency gets more or fewer instances, or it becomes slow.
A limit that is too high does not protect the dependency.
A limit that is too low rejects calls that the dependency can serve.

The concurrency limiter measures the result of each call and adapts the limit.
It decreases the limit quickly when the dependency shows overload.
It increases the limit slowly while the calls succeed under load.
This rule has the name AIMD: additive increase, multiplicative decrease.

## 2. Terms

| Term | Meaning |
|---|---|
| Call in progress | A call that the limiter started and that did not return yet. |
| Limit | The maximum number of calls in progress. The limiter keeps it as a number with a fraction. The effective limit is the integer part. |
| Overload signal | A result of a call that shows overload of the dependency. |
| Busy call | A call that starts when the calls in progress, with this call included, are at least half of the limit. |

## 3. Functional requirements

### Decision

| ID | Requirement |
|---|---|
| CL-1 | The limiter runs a call when the number of calls in progress is below the limit. |
| CL-2 | When the number of calls in progress is equal to or above the limit, the limiter rejects the call immediately with `ErrLimitExceeded`. It does not run the call, and it does not wait. A rejection does not change the limit. |
| CL-3 | When the limit decreases below the number of calls in progress, these calls continue. The limiter rejects new calls until the number is below the limit. |

### Adaptation

| ID | Requirement |
|---|---|
| CL-4 | After a call with an overload signal, the new limit is the larger value of `MinLimit` and `limit × BackoffRatio`. |
| CL-5 | A call is an overload signal when `OverloadIf` returns `true` for its error. By default, this is an error that matches `context.DeadlineExceeded`. |
| CL-6 | When `LatencyThreshold` is set, a call that takes this time or longer is also an overload signal. The limiter measures the time with the configured clock. By default, the latency is not a signal. |
| CL-7 | The limiter keeps the limit as a number with a fraction. The effective limit is its integer part. |
| CL-8 | The limit never decreases below `MinLimit`. |
| CL-9 | After a busy call without an overload signal, the new limit is the smaller value of `MaxLimit` and `limit + 1/limit`. An error of the call that is not an overload signal does not prevent this increase. |
| CL-10 | A call that is not busy does not increase the limit. |
| CL-11 | The limit never increases above `MaxLimit`. |

### Context, errors, and panic

| ID | Requirement |
|---|---|
| CL-12 | The limiter returns the error of the call without a change. |
| CL-13 | When the context has already ended, the limiter returns the error of the context. It does not run the call. |
| CL-14 | The limiter passes the context of the caller to the call. |
| CL-15 | When the call panics, the call is not in progress after that. The panic continues to the caller with the same value. A panic does not change the limit. |
| CL-16 | `ErrLimitExceeded` matches `faultvane.ErrRejected`. |

### Configuration

| ID | Requirement |
|---|---|
| CL-20 | The options are `InitialLimit` (default 20), `MinLimit` (default 1), `MaxLimit` (default 200), `LatencyThreshold` (default off), `BackoffRatio` (default 0.9), `OverloadIf` (default: an ended deadline), and `Clock` (default: the system clock). |
| CL-21 | `New` returns an error that matches `faultvane.ErrInvalidConfig` when a value is not valid. The error names the option. The rules are: `MinLimit` is 1 or more, `MaxLimit` is `MinLimit` or more, `InitialLimit` is from `MinLimit` to `MaxLimit`, `LatencyThreshold` is above 0, `BackoffRatio` is above 0 and below 1, and `OverloadIf` and `Clock` are not nil. |

### API

| ID | Requirement |
|---|---|
| CL-18 | `Limit` returns the effective limit. `InFlight` returns the number of calls in progress. |
| CL-19 | The limiter implements `faultvane.Policy`. It works in a pipeline. |

## 4. Non-functional requirements

| ID | Requirement |
|---|---|
| CL-17 | All methods are safe for concurrent use. Concurrent callers never get more calls in progress than the limit. No change of the limit is lost. |
| CL-22 | The limiter starts no goroutine. |
| CL-23 | A call makes no memory allocation in the limiter. A rejection makes no memory allocation. |

## 5. Statistics

| ID | Requirement |
|---|---|
| CL-24 | `Stats` returns the number of permitted calls, the number of rejected calls, the number of overload signals as decreases of the limit, the effective limit, and the number of calls in progress. To count makes no memory allocation and uses no lock. |
| CL-25 | The OpenTelemetry module exports the values of `Stats` as metrics. The attributes are the name that the application gives to the limiter and the result of the decision. |
