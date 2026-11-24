# ADR 0005: One repository with a dependency-free core module

Status: Proposed

## Context

Faultvane will contain many policies: circuit breaker, timeout, retry, bulkhead, concurrency limiter, rate limiter, and load shedding.
Some functions need external libraries: the Redis client and OpenTelemetry.
An application that uses only a local policy must not get these libraries as dependencies.
The policies share types, and the composition of policies needs them together.

## Decision

One repository contains all Go code.

- The core module is at the repository root. It contains all policies as packages. It depends only on the standard library.
- Each integration with an external library is a separate module in a subdirectory: `store/redis` and `otel`.
- Examples and tests that need external libraries are also separate modules.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| One module for all code | Each user gets the Redis client and OpenTelemetry as dependencies. |
| One module for each policy in one repository | A change of a shared type needs a sequence of releases. Version tags with a prefix for each policy make releases complex. The policies have no heavy dependencies that justify the separation. |
| One repository for each policy | A change across policies needs many coordinated changes. The work for continuous integration and releases multiplies. One maintainer cannot keep this up. |

## Consequences

Positive:

- The import paths are short: `github.com/faultvane/faultvane-go/circuitbreaker`.
- The policies have one version. A user cannot combine versions that do not fit.
- The core has a `go.mod` without requirements.

Negative:

- The repository has more than one module. A release needs a tag for each module, for example `v0.1.0` and `store/redis/v0.1.0`.
- Development across modules needs a Go workspace file. The file is not committed.
- A breaking change in one policy needs a new major version of the full core module.

## Examine again when

- One policy needs a heavy dependency.
- A separate team takes ownership of one policy.
- One policy needs a release schedule that is different from the others.
