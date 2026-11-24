# ADR 0003: Redis server time for all shared timestamps

Status: Proposed

## Context

The shared state contains times: the transition time, the end of the open duration, the buckets of the window, and the end of each lease.
The clocks of the instances are not equal.
If each instance writes its own time, one instance with a wrong clock can end an open duration early or hold a lease too long.

## Decision

All times in the shared state come from the Redis server clock.
The scripts read the time with the `TIME` command.
Each response contains the Redis time.
The instance calculates the difference between the Redis clock and its own monotonic clock, and uses this difference for local decisions about the snapshot.

In local mode, the instance uses only its monotonic clock and durations.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| The time of the instance as a script argument | The result depends on the clock of the instance that writes. Clock differences cause wrong transitions. |
| Durations only, with Redis key expiry as the timer | Key expiry gives no exact time to the instances. The window buckets and the leases need comparable times. |
| A logical clock | The open duration and the lease duration are real durations. A logical clock cannot measure them. |

## Consequences

Positive:

- One clock decides all shared times. The clocks of the instances have no effect on the shared state.
- The scripts are deterministic for a given Redis time.

Negative:

- After a failover, the clock of the new primary can be different. The error is equal to the clock difference between the Redis servers. Time synchronization of the Redis servers keeps it small.
- The estimate of the clock difference in the instance has an error of at most the round trip time of the response.
- Scripts that call `TIME` and then write need a Redis version that replicates the effects of a script. Redis 6.2 satisfies this.

## Examine again when

- A store without a server clock must be supported.
