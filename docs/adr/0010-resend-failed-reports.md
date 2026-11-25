# ADR 0010: A failed report is sent again with the next report

Status: Accepted

## Context

In distributed mode, an instance sends its pending counts to the store as a report (ADR 0001).
A report can fail, for example with a timeout. The instance then does not know if the store added the counts.

Until version 0.1.0, the instance did not send the counts of a failed report again.
The reason was that counts which the store adds two times can open the circuit of a healthy dependency.
ADR 0001 names the result as a negative consequence: "A lost report makes the shared window smaller for a short time."

This rule breaks the main function of the shared breaker (FD-6, AT-1) when many first reports fail at the same time.

- The scenario "AT-1 FD-6 ten pods with one failure each open the circuit with postgres" failed one time in continuous integration. The message was "timeout: all pods are open" after 3 minutes.
- The probable cause: PostgreSQL was cold, and the first reports of 6 or more pods were slower than the store timeout of 80 ms. Each pod had exactly 1 failure. The counts of the failed reports were lost. The shared window then had fewer than 5 failures, and no later call added a failure.
- The same defect occurred before with MongoDB, when the first write created the collection ([scenarios](../scenarios.md), section 6).
- A core test shows the effect without a real store: ten instances have one failure each, and the first report of six instances fails. Without a change, the store stays `CLOSED`.

The report of a reconciliation has the rule of requirement RC-7 already: the instance keeps the counts of a failed report and sends them again.

The limits for a solution are:

- The `Store` interface, the stored data, the Lua scripts, and the adapters are commitments. Version 0.1.0 is released.
- The request path makes no store operation and no allocation.
- A count never goes to a newer generation (GEN-3).
- The number of store operations during a store fault keeps its limit.

## Decision

1. **An instance keeps the counts of a failed report in distributed mode also.** When `Report` returns an error, the synchronizer puts the counts of that report back into the pending counts. The next synchronization sends them in one report, together with the new counts. The instance makes no additional store operation. Requirement FD-9 states the rule.
2. **A synchronization without pending counts does not change.** It reads the state with `Load`.
3. **The fault threshold is the limit.** Each failed report is a store fault. After the fault threshold (default 3) of store faults in sequence, the instance changes to local mode, as before. The kept counts are then pending counts of local mode. The reconciliation sends them with its limit, the totals of the last local `CLOSED` window (RC-7, [degraded mode](../circuit-breaker/degraded-mode.md)).
4. **Kept counts stay in their generation.** A report carries the generation of the snapshot of the instance. The store ignores a report with an old generation. An instance that adopts a different generation discards its pending counts.
5. **The owner accepts counts that the store adds more than one time.** A circuit that does not open is the worse error for a circuit breaker. A circuit that opens too early closes after one successful probe.

Point 4 also closes a path that existed before this decision. A result that an instance recorded during the store operation of a report stayed in the pending counts. When the response of that report brought a newer generation in the `CLOSED` state, the next report sent that result with the newer generation.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| Keep the loss | This is the condition before the decision. The store never adds a count two times. But the circuit can stay `CLOSED` although the instances together reached the threshold, and no later event corrects this while the instances get no new failures. The acceptance test AT-1 then depends on the latency of the first store operations. |
| An idempotency key in the report. The instance gives each report an identifier, and the store ignores a report that it applied before. | This is the exact solution. The store then adds each count exactly one time. But it changes the store contract, the stored data, the Lua scripts, and all four adapters. Each store must keep the identifiers for a time and remove them. Old and new versions must run against the same store during a deployment. The cost is too high for a correction in a minor version. See "Examine again when". |
| Send the counts again one time only, then discard them | The worst case is smaller: the store adds one result at most 2 times. But a second failed report loses the counts again, and the circuit can stay `CLOSED`. The instance also needs a second set of counters to know which counts it sent one time before. The fault threshold gives a limit without new state. |
| Send the counts again immediately in the same synchronization | It adds store operations during a store fault, at the time when the store is slow. The number of store operations for each sync interval then has no fixed limit of one. |
| Put the counts of a failed report directly into the local decision and open the local circuit | One instance does not have the evidence of the other instances. This is the function of local mode after the fault threshold. |

## Consequences

Positive:

- The failures of a failed report reach the shared window with the next successful report. Ten instances with one failure each open the circuit when the first reports of some instances fail.
- The rule for a failed report is the same in distributed mode and in a reconciliation.
- The `Store` interface, the stored data, the scripts, and the adapters do not change. Old and new versions can use the same store.
- The request path does not change. The number of store operations does not change: one for each synchronization.
- The pending counts of an old generation no longer go to a newer generation.

Negative:

- The store can add a count more than one time. This occurs only when the store applied a report and the response did not arrive.
- Worst case with the default fault threshold of 3: the store adds one result 3 times before the instance is in local mode, one time for each failed report. The reconciliation then sends the counts one more time, with the limit of the local window. The total is then 4. Each more report of a reconciliation whose response does not arrive adds one more time (this part existed before, RC-7).
- A failure count threshold and the minimum number of calls are reached earlier. Example: the failure threshold is 5 and an instance has 2 failures. After 3 reports without a response, the shared window has 6 failures from this instance, and the circuit opens for a dependency that had 2 failures. A rate threshold does not change when the store adds the calls and the failures of one instance more than one time.
- A circuit that opens too early rejects calls for one open duration. Then one successful probe closes it.
- When a report opens the circuit, the generation changes and the store ignores the later reports with the old generation. The counts that are too high leave the shared window after one window size.
- A higher fault threshold gives a higher worst case. The worst case in distributed mode is equal to the fault threshold.
- A successful store operation of a different type sets the fault count back to 0. A late result of a probe from an older generation can do this while the circuit is `CLOSED`. The number of failed reports is then higher than the fault threshold. This condition is rare.
- An instance that adopts a different generation discards the results that it recorded for the old generation and did not send. This agrees with GEN-3. A result that the request path adds at the same instant as the generation change can still be in the wrong group. An exact solution needs pending counts for each generation.
- The sentence about a lost report in ADR 0001 is no longer correct. This record replaces that consequence.

## Examine again when

- A user reports a circuit that opened too early because of counts that the store added more than one time.
- The store contract changes for a different reason, or a version 2 of the stored data starts. An idempotency key in the report is then possible at a low additional cost.
- Measurements show that reports are applied without a response frequently, for example with a store timeout that is near the usual store latency.
- An application needs a fault threshold that is much higher than 3.
