# Failure detection

This document describes how the breaker decides that a dependency is unhealthy.

## 1. Result of a call

Each call that the breaker permits gets one of these results:

| Result | Default rule |
|---|---|
| Success | The call returns no error. |
| Ignored | The call returns an error that matches `context.Canceled`. The breaker does not count the call. |
| Failure | The call returns a different error, for example `context.DeadlineExceeded`. A panic of the call is also a failure. |

The function `circuitbreaker.DefaultClassifier` is this rule.
The rule reads only the error that the call returns. A call that returns no error after its deadline is a success.

The application can replace the default rule with a classifier function.
For example, the application can classify a "not found" error as a success.

A call is also a **slow call** when its duration is equal to or longer than the slow call duration.
A slow call can be a success or a failure.
Slow call detection is off by default.

## 2. Failure condition

The circuit opens when one of these conditions is true for the window:

| Condition | Options |
|---|---|
| The number of failures is equal to or larger than the failure count threshold. | `WithFailureThreshold` |
| The number of calls is equal to or larger than the minimum, and the failure rate is equal to or larger than the failure rate threshold. | `WithMinimumCalls`, `WithFailureRateThreshold` |
| The number of calls is equal to or larger than the minimum, and the slow call rate is equal to or larger than the slow call rate threshold. | `WithMinimumCalls`, `WithSlowCallDuration`, `WithSlowCallRateThreshold` |

The failure rate condition is always on.
The other two conditions are off when their thresholds have no value.
The method `Thresholds.Exceeded` is this condition. The local breaker, the in-memory, SQL, and MongoDB adapters use it. The Redis report script has the same condition in Lua.

## 3. Window

The window is a sliding time window.
It has a fixed number of buckets of equal duration.
The defaults are a window of 10 seconds with 10 buckets.

Each bucket has three counters: calls, failures, and slow calls.
The total of the window is the sum of the buckets that are not older than the window size.

The library has no count-based window.
A window of "the last N calls" needs one shared and ordered record of each call.
That record is against requirement FD-7.

## 4. Two windows

Each instance has a **local window**.
All instances together have a **shared window** in the store.

| Window | Location | Content | Use |
|---|---|---|---|
| Local window | Memory of the instance | All calls of this instance | The failure condition in local mode |
| Shared window | The store | The sum of the reports of all instances | The failure condition in distributed mode |

In distributed mode, only the shared window opens the circuit.
This gives the result that requirement FD-6 asks for:
10 instances with one failure each open a circuit that has a threshold of 5.

## 5. Reports

The request path adds each result to the local window and to the **pending counts**.
The pending counts are the results that the instance did not send to the store yet.

The synchronizer sends the pending counts as a report:

```text
Report(generation, instance, sequence, calls, failures, slowCalls, thresholds, windowSize, windowBuckets, openDuration, stateTTL, fingerprint)
```

The report carries the thresholds and the window settings of the instance that sends it. The store keeps no configuration.
The report also carries the configuration fingerprint of the instance. Section 6 describes it.
The instance number and the sequence number are the identity of the report. The section "A failed report" describes them.

The store handles a report in one atomic operation.
The [store contract](../stores/README.md) defines this operation for all adapters.

1. Compare the generation of the report with the stored generation. If they are different, or the state is not `CLOSED`, ignore the counts.
2. Add the counts to the current bucket of the shared window. The store time selects the bucket.
3. Remove the buckets that are older than the window size.
4. Calculate the totals and evaluate the failure condition.
5. If the condition is true, make transition T1.
6. Save the fingerprint of the report. A report that step 1 ignores does not change the stored fingerprint.
7. Return the authoritative state, the store time, and the fingerprint that the store had before this report.

### Time of a report

A report goes into the bucket of the time at which the store receives it.
The error in time is at most one sync interval.
For this reason, the bucket duration must be equal to or longer than the sync interval.
The configuration validation checks this rule.

### A failed report

A report can fail, for example with a timeout.
The instance then does not know if the store added the counts.

The instance keeps a failed report (FD-9). This is the **unconfirmed report**.
The next synchronization sends the unconfirmed report again, without a change: the same counts, the same generation, and the same identity.
It sends no other report. New results stay in the pending counts.
When the store confirms the unconfirmed report, the synchronizer gets the signal for an early report. The next synchronization then sends the pending counts.
A synchronization without an unconfirmed report and without pending counts reads the state and sends no report.
[ADR 0010](../adr/0010-resend-failed-reports.md) and [ADR 0011](../adr/0011-report-identity.md) record these decisions.

The reason is requirement FD-6.
Without this rule, the failures of a failed report never reach the shared window.
When the first reports of many instances fail, the circuit stays `CLOSED`, although the instances together have sufficient failures.
A circuit that does not open is the worse error for a circuit breaker.

The rule has three limits.

1. **The fault threshold.** Each failed report is a store fault. After 3 store faults in sequence, the instance changes to local mode. The option `WithFaultThreshold` sets this number. The instance keeps the unconfirmed report in local mode. The reconciliation sends it again. [Degraded mode](degraded-mode.md) gives the rules.
2. **The generation.** The unconfirmed report carries the generation of the snapshot that the instance had when it made the report. When the store made a transition in the meantime, the store ignores the report (GEN-3). When the instance adopts a different generation, it discards its pending counts and the unconfirmed report. A count never goes into a report with a newer generation.
3. **The window.** The instance discards an unconfirmed report that is as old as the window size. The counts of a report are in the shared window for one window size, as each other count.

### The identity of a report

The store can add the counts of a report and the response can fail to arrive.
The instance then sends the same report again. The store must not add the counts a second time (FD-10).

- Each report carries the number of its instance and a sequence number. The instance number is a random 64-bit number that the breaker selects when it starts. The sequence starts at 1 and increases by 1 for each new report.
- A report that is sent again has the same sequence.
- For each instance, the store keeps the sequence of the last report that it applied. A report with a sequence that is equal to or lower than this value changes nothing. Rule C14 of the [store contract](../stores/README.md) states this.
- The store forgets an instance when the counts of its last report left the shared window. A transition removes all instances, together with the window.
- All adapters of this project satisfy rule C14. A result then reaches the shared window at most one time.

A store from a different source that ignores the identity adds the counts again with each report that is sent again.
With the default fault threshold of 3, such a store adds one result at most 3 times before the instance is in local mode, and one more time for each report of a reconciliation whose response does not arrive.
A failure count threshold and the minimum number of calls are then reached too early.
An instance of version 0.2.0 or older sends no identity. During a deployment with mixed versions, this limit stays for the old instances. [ADR 0011](../adr/0011-report-identity.md) gives the details.

The local window is not affected. It contains each result one time.

## 6. Configuration differences between instances

The failure condition uses the thresholds of the instance that sends the report.
During a rolling deployment, instances can have different thresholds for a short time.
The breaker continues to operate.

The library detects this difference with a **configuration fingerprint**.
The detection only tells the operator. It changes no permit decision, it fails no call, and it blocks no call.
[ADR 0008](../adr/0008-configuration-fingerprint.md) records this decision.

### Values in the fingerprint

The fingerprint contains the values that must be equal in all instances that share one breaker.

| Key in the canonical text | Option | Value |
|---|---|---|
| `failure_threshold` | `WithFailureThreshold` | The number. `0` when the option is off. |
| `minimum_calls` | `WithMinimumCalls` | The number. |
| `failure_rate` | `WithFailureRateThreshold` | The rate as the shortest decimal text, for example `0.5`. |
| `slow_call_rate` | `WithSlowCallRateThreshold` | The rate as the shortest decimal text. `0` when the option is off. |
| `slow_call_duration` | `WithSlowCallDuration` | Nanoseconds. `0` when the option is off. |
| `window_size` | `WithWindowSize` | Nanoseconds. |
| `window_buckets` | `WithWindowBuckets` | The number. |
| `open_duration` | `WithOpenDuration` | Nanoseconds. |
| `half_open_max_probes` | `WithHalfOpenMaxProbes` | The number. |
| `half_open_success_threshold` | `WithHalfOpenSuccessThreshold` | The effective number. Without the option, it is the probe limit. |
| `probe_timeout` | `WithProbeTimeout` | Nanoseconds. |
| `state_ttl` | `WithStateTTL` | The effective state TTL in nanoseconds. |

These values are not in the fingerprint, because each instance can have its own value:
the name of the breaker, the store, the logger, the observer, the clock, the classifier, the store timeout, the sync interval, the early report interval, the fault threshold, the start timeout, the probe attempt interval, the start policy, and all options of local mode.

A classifier is a function. The library cannot compare two functions. Two instances with different classifiers have the same fingerprint.

### Format

The breaker writes the values as one canonical text. The text starts with the version of the format. The order of the keys is fixed.

```text
v1|failure_threshold=0|minimum_calls=20|failure_rate=0.5|slow_call_rate=0|slow_call_duration=0|window_size=10000000000|window_buckets=10|open_duration=30000000000|half_open_max_probes=1|half_open_success_threshold=1|probe_timeout=30000000000|state_ttl=3600000000000
```

The fingerprint is the version, a colon, and the first 8 bytes of the SHA-256 hash of the canonical text as 16 hexadecimal characters.
The text above is the default configuration. Its fingerprint is `v1:c0183f3821e11064`.

`breaker.ConfigFingerprint()` returns the fingerprint of an instance.
A change of the list of values or of the text needs a new version. Instances with different versions of the format report a difference.

### Detection

1. Each report and each probe operation carries the fingerprint of the instance that sends it.
2. The store saves the fingerprint of each operation that it accepts. The last writer wins.
3. Each response carries the fingerprint that the store had before the operation. `Load` gives the current fingerprint.
4. After each response, the instance compares that fingerprint with its own fingerprint.
5. An empty fingerprint means "not known". It is never a difference. Data from before this function and a new breaker have no fingerprint.

Each instance keeps one condition: "a writer has a different configuration".

- The condition starts at the first response with a fingerprint that is known and different.
- The condition ends after 10 responses in sequence that have no different fingerprint. An empty fingerprint counts as no difference.
- One response with a different fingerprint sets the count back to zero.

The number 10 is a constant of the library. It is not an option.
An instance that writes gets its own fingerprint in its next response. Without this rule, the condition would change at almost each synchronization in a fleet with two configurations.

When the condition starts or ends, the instance sends one event and writes one log.
`Stats().ConfigMismatch` and the metric `config_mismatch` show the current condition. [Observability](observability.md) describes these signals.

### Limits of the detection

- The store keeps one fingerprint. An instance compares with the last writer only, not with each instance.
- In a fleet with two configurations, the condition of an instance stays while the other group writes at least one time in each 10 responses that the instance gets. When the other group writes less frequently, the condition can end and start again. Each change gives one event and one log.
- The condition ends 10 responses after the last different fingerprint. With the default sync interval, that is approximately 5 seconds after the configurations became equal.
- An instance finds a difference only after an instance with a different configuration wrote. An instance writes when it sends a report with counts or does a probe operation. A group of instances without traffic is not visible to the other group.
- The fingerprint does not change while the circuit is `OPEN` and no probe operation occurs.
- In local mode, the instance gets no response. The condition and the count keep their last values.

## 7. Limits of the design

- The shared window is the sum of reports, not an exact record of each call. Its error in time is at most one sync interval.
- Between the time at which the shared window reaches the threshold and the next report, the circuit is still `CLOSED`. A recorded failure causes an early report to keep this time short.
- An instance that cannot reach the store does not add to the shared window. It protects the dependency with its local window.
