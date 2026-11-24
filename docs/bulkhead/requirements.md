# Bulkhead requirements

This document lists what the bulkhead must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The tests refer to these identifiers.

## 1. Problem

A service calls many dependencies.
When one dependency becomes slow, its calls stay in progress for a long time.
Each call in progress holds a goroutine, memory, and frequently a connection.
Without a limit, the slow dependency uses all these resources.
Then the calls to healthy dependencies also fail.

The bulkhead gives each dependency a fixed number of slots.
A call holds one slot while it runs.
A slow dependency can use only its own slots.

## 2. Terms

| Term | Meaning |
|---|---|
| Slot | The permission to run one call. The bulkhead has `MaxConcurrent` slots. |
| Call in progress | A call that holds a slot. |
| Waiting caller | A caller that waits for a free slot. |
| Waiting place | The permission to wait. The bulkhead has `MaxWaiting` waiting places. |

## 3. Functional requirements

### Slots

| ID | Requirement |
|---|---|
| BH-1 | The bulkhead runs a call when a slot is free. The call holds the slot until it returns. |
| BH-2 | The number of calls in progress never exceeds `MaxConcurrent`. |
| BH-3 | When all slots are in use and no waiting place is free, the bulkhead rejects the call immediately with `ErrFull`. It does not run the call. |

### Waiting

| ID | Requirement |
|---|---|
| BH-4 | When all slots are in use and a waiting place is free, the caller waits. The caller gets a slot when a call in progress returns. |
| BH-5 | Waiting callers get a slot in the order of their arrival. |
| BH-6 | A waiting caller whose `MaxWait` ends gets `ErrFull`. The bulkhead does not run its call. |
| BH-7 | Without `MaxWait`, a waiting caller waits until it gets a slot or its context ends. |
| BH-8 | A waiting caller whose context ends gets the error of the context. The bulkhead does not run its call. The waiting place becomes free. |
| BH-20 | A waiting caller that gets a slot at the same moment that its context or its `MaxWait` ends runs its call. A slot that the bulkhead gives is never lost. |
| BH-22 | The number of waiting callers never exceeds `MaxWaiting`. The memory of a bulkhead has a fixed limit. |

### Context, errors, and panic

| ID | Requirement |
|---|---|
| BH-9 | When the context has already ended, the bulkhead returns the error of the context. It does not run the call, and it uses no slot and no waiting place. |
| BH-10 | The bulkhead passes the context of the caller to the call. |
| BH-11 | The bulkhead returns the error of the call without a change. The slot becomes free. |
| BH-12 | When the call panics, the slot becomes free. The panic continues to the caller with the same value. |
| BH-13 | `ErrFull` matches `faultvane.ErrRejected`. |

### Configuration

| ID | Requirement |
|---|---|
| BH-14 | The options are `MaxConcurrent` (default 10), `MaxWaiting` (default 0), and `MaxWait` (default 0, which means no limit). |
| BH-15 | `New` returns an error that matches `faultvane.ErrInvalidConfig` when `MaxConcurrent` is below 1, `MaxWaiting` is below 0, or `MaxWait` is below 0. The error names the option. |

### API

| ID | Requirement |
|---|---|
| BH-16 | `InFlight` returns the number of calls in progress. `Waiting` returns the number of waiting callers. |
| BH-21 | The bulkhead implements `faultvane.Policy`. It works in a pipeline. |

## 4. Non-functional requirements

| ID | Requirement |
|---|---|
| BH-17 | All methods are safe for concurrent use. |
| BH-18 | The bulkhead starts no goroutine. |
| BH-19 | A call that finds a free slot makes no memory allocation in the bulkhead. A rejection makes no memory allocation. |
