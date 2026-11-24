# Requirements

This document lists the requirements for the Faultvane toolkit.
They apply to each policy.
Each policy also has its own requirements document.

## 1. Purpose

A service in production depends on other systems.
These systems become slow, fail, or receive too much traffic.
Without protection, one failure spreads from service to service.

Faultvane gives a Go service the policies that stop this spread:

| Policy | Purpose | Requirements |
|---|---|---|
| Circuit breaker | Stop calls to a dependency that fails. | [Circuit breaker requirements](circuit-breaker/requirements.md) |
| Timeout | Limit the time of an attempt. | Written before its milestone |
| Retry | Repeat an attempt that failed for a short time. | Written before its milestone |
| Bulkhead | Limit the resources that one dependency can use. | Written before its milestone |
| Concurrency limiter | Limit the calls that run at the same time, and adapt the limit. | Written before its milestone |
| Rate limiter | Limit the calls in a time period. | Written before its milestone |
| Load shedding | Reject low-priority work when the service is overloaded. | Written before its milestone |

## 2. Goals

1. Prevent cascading failures.
2. Protect dependencies and the service itself from overload.
3. Behave correctly under concurrency and under faults.
4. Recover automatically.
5. Add very low latency to each call.
6. Be simple to use correctly.

The order of this list is the priority order.

## 3. Requirements for all policies

### Structure

| ID | Requirement |
|---|---|
| TK-1 | Each policy is usable alone. It does not import a different policy. |
| TK-2 | All policies implement one policy interface. |
| TK-3 | A pipeline applies many policies to one call in the order that the application gives. |
| TK-4 | The documentation defines the recommended order of the policies and their interaction. |
| TK-5 | The core module depends only on the Go standard library. |
| TK-6 | Each integration with an external library is a separate module. |

### Behavior

| ID | Requirement |
|---|---|
| TK-10 | A policy makes its decision from local memory. A normal call makes no network round trip in a policy. |
| TK-11 | A rejection by a policy matches one common error. The application can tell a rejection from an error of the call. |
| TK-12 | A policy returns the error of the call without a change. |
| TK-13 | Each function that can block accepts a `context.Context` and obeys cancellation. |
| TK-14 | All public methods are safe for concurrent use. |
| TK-15 | A policy starts no goroutine that it does not stop. |
| TK-16 | Memory use has a fixed limit for each policy instance. |
| TK-17 | A panic of the call counts as a failure. The policy panics again with the same value. |

### Configuration

| ID | Requirement |
|---|---|
| TK-20 | Each option has a safe default. |
| TK-21 | An invalid configuration fails when the application creates the policy. |

### Shared state

| ID | Requirement |
|---|---|
| TK-30 | A policy that can coordinate instances works also without a shared store. |
| TK-31 | A policy uses a shared store only through an interface. Adapters implement the interface for different stores. |
| TK-32 | The shared store is not a single point of failure. When the store is not available, the policy continues with local state. |
| TK-33 | A fault of the store never fails an application call and never blocks it without a limit. |
| TK-34 | A local state never overwrites a newer state in the store. |
| TK-35 | The load on the store does not depend on the request rate of the application. |

### Observability

| ID | Requirement |
|---|---|
| TK-40 | Each policy exposes metrics for its decisions and its state. |
| TK-41 | Metric labels do not have high cardinality. |
| TK-42 | Each policy logs its state changes and its faults. It does not log each call. |
| TK-43 | Logs, metrics, and traces contain no credentials, request payloads, or tokens. |
| TK-44 | Tracing is optional. It adds no significant cost when it is off. |

### Performance

| ID | Requirement |
|---|---|
| TK-50 | The decision of a policy on its normal path makes no memory allocation. |
| TK-51 | A policy adds less than 1 ms to a call in normal conditions. Each policy has a stricter measured target. |
| TK-52 | Lock contention is not a bottleneck under high concurrency. |
| TK-53 | Each performance statement in the documentation comes from a benchmark in the repository. |

## 4. Out of scope

- Service discovery and load balancing.
- A proxy or a sidecar. Faultvane is a library.
- Implementations in other languages.
