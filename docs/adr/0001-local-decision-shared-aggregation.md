# ADR 0001: Local decisions with batched shared aggregation

Status: Proposed

## Context

The breaker must open when all instances together see sufficient failures (FD-6).
The request path must not call Redis for each call (PF-1, FD-7).
Redis traffic must not increase with the request rate (PF-3).

These requirements pull in different directions.
Exact shared counts need a write for each call.
Fully local counts do not solve the original problem.

## Decision

Each instance decides from a snapshot in memory.
Each instance counts results locally and sends the counts to Redis in batches.
Redis adds the batches to a shared window and evaluates the failure condition in the same atomic script.

A recorded failure causes an early report, with a rate limit.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| A Redis read and write for each call | It adds network latency to each call and makes Redis a bottleneck. The requirements forbid it. |
| Local evaluation only. An instance that reaches the threshold alone opens the shared circuit. | With many instances, no instance reaches a count threshold alone. This is the original problem. A rate threshold with `MinimumCalls` has the same problem at low traffic. |
| Gossip between instances without a central store | It needs membership and a protocol between instances. Atomic probe coordination is difficult. The complexity is not justified. |
| A stream of single results to Redis, written asynchronously | The Redis load still increases with the request rate. |

## Consequences

Positive:

- The `CLOSED` path uses memory only.
- The Redis load depends on the number of instances and breakers and on the intervals.
- The circuit opens on the evidence of all instances.

Negative:

- The shared window has an error in time of at most one sync interval.
- A transition reaches the other instances with a delay of at most one sync interval.
- A lost report makes the shared window smaller for a short time.

## Examine again when

- Measurements show that the propagation delay lets too much traffic reach an unhealthy dependency.
- An application needs a breaker for a dependency with a very low call rate, where batches give no benefit.
