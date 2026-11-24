# Retry requirements

This document lists what the retry policy must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

Some failures are short: a lost connection, a restart of one instance, a short overload.
A second attempt frequently succeeds.

But retries are dangerous during an outage.
Three attempts for each request give three times the traffic to a dependency that already fails.
A retry policy must help with short failures and must not multiply traffic during an outage.

## 2. Terms

| Term | Meaning |
|---|---|
| Attempt | One run of the call. |
| First attempt | The first run of the call for one `Do`. |
| Retry | Each attempt after the first attempt. |
| Final error | An error after which the policy makes no retry. |
| Backoff | The wait before a retry. |
| Gate | A function of the application that permits or stops retries. |
| Budget | A limit on the number of retries in relation to the number of first attempts. |
| Token | The unit of the budget. One retry uses one token. |

## 3. Functional requirements

### Attempts and result

| ID | Requirement |
|---|---|
| RT-1 | The retry implements the policy interface of the toolkit. It works in a pipeline and with `faultvane.Execute`. |
| RT-2 | The policy runs the call again after an error, up to `MaxAttempts` attempts. The number includes the first attempt. The default is 3. |
| RT-3 | When an attempt returns nil, `Do` returns nil immediately and makes no more attempts. |
| RT-4 | When the policy stops, `Do` returns the error of the last attempt without a change. Each attempt gets the context of the caller. |

### Retry rule

| ID | Requirement |
|---|---|
| RT-5 | An error that matches `faultvane.ErrRejected` is a final error. This rule applies also with a custom retry rule. The policy does not ask the custom retry rule about a rejection. |
| RT-6 | When the context of the caller ended, the error of the attempt is a final error. The first attempt always runs. |
| RT-7 | The application can give a custom retry rule with `WithRetryIf`. When the function returns `false` for an error, this error is a final error. The default rule tries each other error again. |

### Backoff

| ID | Requirement |
|---|---|
| RT-8 | Before each retry, the policy waits for a random duration from 0 to below a ceiling. The ceiling of the first retry is the initial duration. It doubles for each subsequent retry up to the maximum duration. The defaults are 100 ms and 10 s. |
| RT-9 | The wait stops immediately when the context of the caller ends. `Do` then returns the error of the last attempt, not the error of the context. |

### Gate

| ID | Requirement |
|---|---|
| RT-10 | The application can give a gate with `WithGate`. The policy calls the gate one time before each retry. When the gate returns `false`, the policy stops and returns the error of the last attempt. The policy does not call the gate before the first attempt. |

### Budget

| ID | Requirement |
|---|---|
| RT-11 | The budget is off by default. Without a budget, only `MaxAttempts` limits the retries. |
| RT-12 | With `WithBudget(ratio)`, the policy keeps a number of tokens. A new policy has 10 tokens. Each first attempt adds `ratio` tokens. The policy keeps 10 tokens at most. A retry adds no token. |
| RT-13 | Each retry removes one token. Without a full token, the policy stops and returns the error of the last attempt. A final error and a closed gate remove no token. |

### Configuration

| ID | Requirement |
|---|---|
| RT-14 | `New` validates each option. For an invalid configuration, `New` returns no policy and an error that matches `faultvane.ErrInvalidConfig`. |
| RT-15 | Each option has a default. `New()` without options gives a policy that works. |

### Concurrency, goroutines, and panic

| ID | Requirement |
|---|---|
| RT-16 | All methods are safe for concurrent use. Under contention, the budget never permits more retries than the tokens that it has. |
| RT-17 | The policy starts no goroutine. The wait runs in the goroutine of the caller. |
| RT-18 | A panic of the call continues to the caller with the same value. The policy does not try the call again after a panic. |

### Performance and memory

| ID | Requirement |
|---|---|
| RT-19 | A request with a successful first attempt makes no allocation in the policy and uses no lock. The decisions of the policy after a failed attempt make no allocation. |
| RT-20 | The memory of a policy instance has a fixed size. It does not grow with the number of calls. |

### Statistics

| ID | Requirement |
|---|---|
| RT-21 | `Stats` returns the number of calls, the number of retries, the number of calls that returned an error, the number of stops by the gate, and the number of stops by an empty budget. To count makes no memory allocation and uses no lock. |
| RT-22 | The OpenTelemetry module exports the values of `Stats` as metrics. The only attribute is the name that the application gives to the policy. |
