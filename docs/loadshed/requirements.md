# Load shedding requirements

This document lists what the load shedding policy must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

A service has a limit on the work that it can do at one time.
Above this limit, each request becomes slow, and then all requests fail.
The load shedding policy protects the service itself.
When the service is overloaded, the policy rejects low-priority work first.
The service then has capacity for the important work.

## 2. Scope

The policy measures the load of one process.
The overload is local, so the policy has no shared state.

## 3. Terms

| Term | Meaning |
|---|---|
| Shedder | One instance of the load shedding policy. |
| Call in progress | A call that the shedder admitted and that has not ended. |
| Maximum | The largest number of calls in progress that the shedder permits. |
| Utilization | The number of calls in progress divided by the maximum. |
| Priority | The importance of a call. The levels are `Critical`, `High`, `Normal`, and `Low`. |
| Threshold | The utilization at or above which the shedder rejects the calls of one priority. |

## 4. Behavior

| ID | Requirement |
|---|---|
| LS-1 | The shedder implements the policy interface of the toolkit. It runs an admitted call one time and returns the result of the call without a change. |
| LS-2 | The load measure is the number of calls in progress through this shedder. The utilization is this number divided by the maximum. |
| LS-3 | The priority of a call comes from its context. `WithPriority` returns a context with a priority. `PriorityFrom` returns the priority of a context. The four levels are `Critical`, `High`, `Normal`, and `Low`. |
| LS-4 | A context without a priority has the priority `Normal`. A value that is not one of the four levels is also `Normal`. |
| LS-5 | The shedder rejects a call when the utilization is at or above the threshold of the priority of the call. It admits the call when the utilization is below that threshold. |
| LS-6 | A rejected call gets the error `ErrShed`. The shedder does not run a rejected call and does not count it as a call in progress. |
| LS-7 | The error `ErrShed` matches `faultvane.ErrRejected`. |
| LS-8 | With the default thresholds, the shedder rejects `Critical` work only when the number of calls in progress is at the maximum. |
| LS-9 | The number of calls in progress never exceeds the maximum. |
| LS-10 | The method `InFlight` returns the number of calls in progress. A call stops being in progress when it returns, with or without an error. |

## 5. Configuration

| ID | Requirement |
|---|---|
| LS-11 | The default thresholds are 0.5 for `Low`, 0.75 for `Normal`, 0.9 for `High`, and 1.0 for `Critical`. |
| LS-12 | The option `WithMaxInFlight` sets the maximum. The default is 100. The value must be 1 or more. |
| LS-13 | The option `WithThreshold` sets the threshold of one priority. The value must be above 0 and at most 1. The priority must be one of the four levels. |
| LS-14 | The thresholds must not decrease from `Low` to `Critical`. Two priorities can have the same threshold. |
| LS-15 | An invalid configuration makes `New` return an error that matches `faultvane.ErrInvalidConfig`. The error names the option and the rule. |

## 6. Context, concurrency, and panic

| ID | Requirement |
|---|---|
| LS-16 | When the context has ended before the call starts, `Do` returns the error of the context. It does not run the call and it does not count the call as in progress. |
| LS-17 | A panic of the call continues to the caller with the same value. The call stops being in progress. |
| LS-18 | All methods are safe for concurrent use. The limit of LS-9 applies also when many goroutines start calls at the same time. |
| LS-19 | The shedder starts no goroutine. |

## 7. Performance

| ID | Requirement |
|---|---|
| LS-20 | An admitted call and a rejected call make no memory allocation in the shedder. To read the priority of a context makes no memory allocation. |

## 8. Statistics

| ID | Requirement |
|---|---|
| LS-21 | `Stats` returns the number of admitted calls and the number of rejected calls for each priority, and the number of calls in progress. To count makes no memory allocation and uses no lock. |
| LS-22 | The OpenTelemetry module exports the values of `Stats` as metrics. The attributes are the name that the application gives to the shedder, the result of the decision, and the priority. |

## 9. Out of scope

- A load measure from CPU use, memory use, latency, or queue length.
- A queue for rejected work.
- A limit that adapts to measurements. The concurrency limiter has this function.
