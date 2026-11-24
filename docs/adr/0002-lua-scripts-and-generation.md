# ADR 0002: Atomic transitions in Lua scripts with a generation

Status: Proposed

## Context

Many instances change the shared state at the same time.
Only valid transitions are permitted (SM-7).
A stale instance must not overwrite a newer state (GEN-3).
A transition reads the state, reads the window or the leases, and writes many keys.
All these steps must be one atomic operation (GEN-2).

## Decision

Each change of the shared state is one Lua script.
The state has a generation that increases by 1 at each transition.
Each result that an instance sends carries the generation of its permit.
A script ignores a result with another generation.

The three keys of one breaker share a Redis Cluster hash tag.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| `WATCH`, `MULTI`, `EXEC` | It needs more round trips. It fails and must run again under contention. Contention is highest at the moment of an outage, when the breaker must be fast. |
| A distributed lock around read and write | It adds lock expiry and ownership problems. It needs more round trips. A script gives atomic behavior without a lock. |
| Redis Functions | They need Redis 7 and a deployment step for the library code. Scripts with `EVALSHA` work on more versions and need no deployment step. |
| Last write wins without a generation | A late result can close an open circuit or open a closed circuit. |

## Consequences

Positive:

- Each transition is atomic and uses one round trip.
- Concurrent events get a total order.
- The generation gives optimistic concurrency without retries: a stale write has no effect, and the response gives the instance the current state.

Negative:

- A part of the logic is in Lua. It needs its own tests against a real Redis.
- The Go state machine and the scripts must agree. The state machine specification is the reference for both.
- A script blocks Redis while it runs. The scripts must stay small, with a fixed limit on the work for each call.

## Examine again when

- The minimum Redis version becomes 7 or later, and Redis Functions give a clear benefit.
- A second store that has no scripts must be supported.
