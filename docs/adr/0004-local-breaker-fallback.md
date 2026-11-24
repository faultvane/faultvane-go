# ADR 0004: Local breaker as the fallback

Status: Accepted

## Context

Redis must not be a single point of failure (DG-1 to DG-7).
The library must define the behavior in three situations:

1. Redis becomes unavailable during operation.
2. Redis stays unavailable for longer than the maximum time of local mode.
3. An instance starts and cannot read the authoritative state.

Two goals disagree here.
The first goal is to protect the dependency.
The second goal is to keep the application available when only Redis has a fault.

## Decision

1. **During a store fault,** each instance runs the same state machine on local state. The local state starts from the last snapshot.
2. **After the maximum time,** the default policy `KeepLocalBreaker` continues the local breaker and reports an error through logs and metrics. `RejectAll` and `AllowAll` are available.
3. **At a start without Redis,** the default policy `StartWithProbe` starts the local breaker in `HALF_OPEN`. `StartClosed` and `StartRejecting` are available.
4. **At recovery,** the instance adopts the authoritative state. It adds only its pending counts as a normal report. It never writes its local state.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| Reject all calls when Redis is unavailable | A Redis fault then stops all calls to all dependencies. Redis becomes a single point of failure. |
| Permit all calls when Redis is unavailable | The dependency has no protection during a Redis fault. Two faults at the same time then cause a cascading failure. |
| Reject all calls after the maximum time, as the default | It is the safest choice for the dependency. But a long Redis fault then becomes a full outage of the application, and the local breaker already protects the dependency. It stays available as an option. |
| Start in `CLOSED` without Redis, as the default | An instance that starts during an outage sends traffic until its local window reaches the threshold. A probe finds the same answer with one call. |
| Write the local state to Redis at recovery, when the generation is equal | Many instances with different local states then write in a race. The result depends on the order. A report through the normal path gives a result that depends on the sum of the evidence. |

## Consequences

Positive:

- A Redis fault does not stop the application and does not remove the protection of the dependency.
- The authoritative state changes only through the normal atomic scripts. Reconciliation needs no special write path.
- The result of reconciliation is deterministic.

Negative:

- In local mode, instances are not coordinated. A failure count threshold can be too high for one instance. The local overrides reduce this problem.
- In local mode, each instance sends its own probe. A dependency that recovers gets up to one probe for each instance.
- With `StartWithProbe`, the first call after a start without Redis is a probe, and concurrent calls are rejected until it ends.

## Examine again when

- Users report that the default policies do not fit most deployments.
- Measurements show that probes of many instances in local mode overload a dependency that recovers.
