# Timeout requirements

This document lists what the timeout policy must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

A dependency can become slow.
A call without a time limit then holds a goroutine and a connection for a long time.
When many calls wait, the application has no resources for other work.

The timeout policy gives each call a time limit.

## 2. Terms

| Term | Meaning |
|---|---|
| Time limit | The maximum duration of one call. The application sets it when it creates the policy. |
| Context of the caller | The context that the application gives to `Do`. |
| Context of the call | The context that the policy gives to the call. |

## 3. Functional requirements

### Structure and configuration

| ID | Requirement |
|---|---|
| TO-1 | The timeout implements the policy interface of the toolkit. It works in a pipeline and with `faultvane.Execute`. |
| TO-2 | `New` accepts one time limit. The time limit must be above 0. For a different value, `New` returns no policy and an error that matches `faultvane.ErrInvalidConfig`. |

### Call

| ID | Requirement |
|---|---|
| TO-3 | `Do` runs the call exactly one time. |
| TO-4 | The context of the call comes from the context of the caller. It has the values of the context of the caller. It ends with `context.DeadlineExceeded` when the time limit ends. |
| TO-5 | The time limit never extends the deadline of the caller. The earlier of the two deadlines applies. |

### Result

| ID | Requirement |
|---|---|
| TO-6 | When the call returns nil, `Do` returns nil. This rule applies also after the time limit ended. |
| TO-7 | When the call returns an error after the time limit of the policy ended, `Do` returns an error that matches `ErrTimeout` and the error of the call. `errors.Is` and `errors.As` find the error of the call. |
| TO-8 | When the call returns an error before the time limit ended, `Do` returns the error of the call without a change. |
| TO-9 | When the context of the caller ended, `Do` returns the error of the call without a change. A cancellation by the caller and the deadline of the caller are not a timeout of the policy. |
| TO-10 | `ErrTimeout` does not match `faultvane.ErrRejected`, because the call ran. |

### Goroutines and resources

| ID | Requirement |
|---|---|
| TO-11 | `Do` runs the call in the goroutine of the caller. The policy starts no goroutine. |
| TO-12 | The context of the call ends when `Do` returns. The policy keeps no timer after the call. |
| TO-13 | A panic of the call continues to the caller with the same value. The context of the call ends also in this condition. |

### Concurrency and performance

| ID | Requirement |
|---|---|
| TO-14 | All methods are safe for concurrent use. A policy instance has no state that changes. |
| TO-15 | A call makes 4 allocations or fewer in the policy. The allocations are for the context of the call. The policy uses no lock. |
