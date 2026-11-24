# ADR 0008: Configuration fingerprint with the last writer in the shared state

Status: Proposed

## Context

Instances that share one breaker have the same namespace and the same breaker name.
Each report carries the thresholds and the window settings of the instance that sends it.
The store keeps no configuration.

When two instances have different configurations, the failure condition depends on the instance that sends the report.
The same is true for the probe limit, the success threshold, and the open duration.
This condition occurs during each rolling deployment that changes a value. It also occurs after a configuration error.

Before this decision, the library did not detect the condition.
The documentation told the application to use one configuration.
An operator had no signal for a circuit that opens at different thresholds.

The limits for a solution are:

- The request path makes no store operation in the `CLOSED` state and allocates no memory.
- The store interface has one operation for each event. The store keeps no configuration and has no list of instances.
- Old and new library versions must run against the same store at the same time.
- A configuration difference during a rolling deployment is normal. It must not stop calls.

## Decision

1. **Fingerprint.** Each breaker calculates one configuration fingerprint when the application creates it. The fingerprint is a version prefix and the first 8 bytes of the SHA-256 hash of a canonical text, for example `v1:c0183f3821e11064`. The canonical text contains the values that must be equal in all instances: the failure count threshold, the minimum calls, the failure rate, the slow call rate, the slow call duration, the window size, the number of buckets, the open duration, the probe limit, the success threshold, the probe timeout, and the effective state TTL. [Failure detection](../circuit-breaker/failure-detection.md) gives the exact text.
2. **Local values are not in the fingerprint.** The logger, the observer, the clock, the classifier, the store, the synchronization intervals and timeouts, the start policy, and the options of local mode can be different in each instance.
3. **The last writer wins.** The shared state keeps one fingerprint. `Report`, `AcquireProbe`, and `CompleteProbe` carry the fingerprint of the instance. The store saves it when it accepts the operation. An operation that the store ignores and `Restore` do not change it. A transition keeps it.
4. **The response carries the fingerprint from before the operation.** The fingerprint after a write is always the fingerprint of the writer, so it gives no information. `Load` gives the current fingerprint.
5. **An empty fingerprint means "not known".** It is never a difference. Data from before this decision and a new breaker have no fingerprint.
6. **Detection only.** After each response of the store, the instance compares the fingerprint of the response with its own fingerprint. It keeps one condition. The condition starts at the first response with a fingerprint that is known and different. It ends only after 10 responses in sequence without a different fingerprint. One response with a different fingerprint sets the count back to zero. The number 10 is an unexported constant, not an option. When the condition starts or ends, the instance sends the event `ConfigMismatchChanged` and writes one log. `Stats().ConfigMismatch` and the metric `faultvane.circuit_breaker.config_mismatch` show the condition. The breaker changes no permit decision, fails no call, and blocks no call.
7. **Each adapter stores one more value.** The type `Record` has the field `Fingerprint`. The in-memory, SQL, and MongoDB adapters get it through `Record`. The Redis state hash has the field `fp`. Contract rule C13 and the conformance suite define the behavior for each adapter.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| The first writer wins. The store keeps the fingerprint of the first instance until the data expires. | After a deployment, all instances have the new configuration, and the store still has the old fingerprint. Each instance then reports a difference that does not exist. A correction needs a manual step or a rule for when the store replaces the fingerprint. |
| The writer of the last transition wins. The store saves the fingerprint only at a transition. | A breaker that stays `CLOSED` makes no transition. The store then has no fingerprint, or an old one, for a long time. The detection would start only after an outage, when the difference already had an effect. |
| No detection. The documentation tells the application to use one configuration. | This was the condition before the decision. The operator gets no signal. A configuration error stays hidden until a circuit opens at an unexpected threshold. |
| Keep the fingerprint of each instance in the store. | The store needs a list of instances with a cleanup for instances that stopped. This is more data in each write and one more source of defects. The store interface has no identity for an instance. |
| End the condition at the first response without a different fingerprint. | An instance that writes gets its own fingerprint in its next response. In a fleet with two configurations, the condition then changes at almost each synchronization, and each change gives a log. |
| Reject the write of an instance with a different fingerprint. | A rolling deployment would stop the reports of one group of instances. That makes the shared window wrong. The detection must not change the behavior of the breaker. |

## Consequences

Positive:

- An operator gets an event, a log, a statistic, and a metric for a configuration difference.
- The request path does not change. A call in the `CLOSED` state makes no store operation and allocates no memory.
- The store needs no migration. Old data without a fingerprint is valid.
- Old and new library versions can use the same data. An old version ignores the fingerprint. A record that an old version writes through `Record` has no fingerprint, and that is "not known".
- The version prefix lets the format change later.

Negative:

- A fleet with two configurations reports a difference during a rolling deployment, until all instances have the new configuration. This report is correct, and it is also noise for a planned deployment.
- The store keeps only the last writer. The rule of 10 responses keeps the condition stable only while the other group writes at least one time in each 10 responses of the instance. When the other group writes less frequently, the condition can end and start again. Each change gives one event and one log.
- The end of a difference is late by 10 responses. With the default sync interval, that is approximately 5 seconds.
- The number 10 is a fixed value. A fleet with a small group of instances that writes rarely can need a different value.
- The detection needs writes from each group. An instance writes when it sends a report with counts or does a probe operation. A group without traffic is not visible to the other group. An instance without traffic still reads the fingerprint of the last writer at each synchronization.
- Each report, each probe request, and each probe result carries one more value. Each stored state has one more field of 19 characters.
- The fingerprint does not contain the classifier. Two instances with different classifiers have the same fingerprint.
- A change of the fingerprint format in a new library version gives a difference report during the deployment of that version.
- `Record.Fingerprint`, the field `fp` of the Redis hash, and the field `Fingerprint` of the stored JSON are stored data. After version 1.0 they are a compatibility commitment.

## Examine again when

- The condition still starts and ends many times in real fleets. A possible change is a larger number of responses or a time limit in place of a count.
- Operators need the list of the instances that have a different configuration, not only the fact that a difference exists.
- A value moves between the group of shared values and the group of local values. That change needs a new version of the format.
