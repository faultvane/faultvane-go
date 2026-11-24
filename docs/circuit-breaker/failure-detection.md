# Failure detection

This document describes how the breaker decides that a dependency is unhealthy.

## 1. Result of a call

Each call that the breaker permits gets one of these results:

| Result | Default rule |
|---|---|
| Success | The call returns no error. |
| Failure | The call returns an error, or the deadline of its context ends. |
| Ignored | The caller cancels the context. The breaker does not count the call. |

The application can replace the default rule with a classifier function.
For example, the application can classify a "not found" error as a success.

A call is also a **slow call** when its duration is equal to or longer than the slow call duration.
A slow call can be a success or a failure.
Slow call detection is off by default.

## 2. Failure condition

The circuit opens when one of these conditions is true for the window:

| Condition | Options |
|---|---|
| The number of failures is equal to or larger than the failure count threshold. | `FailureThreshold` |
| The number of calls is equal to or larger than the minimum, and the failure rate is equal to or larger than the failure rate threshold. | `MinimumCalls`, `FailureRateThreshold` |
| The number of calls is equal to or larger than the minimum, and the slow call rate is equal to or larger than the slow call rate threshold. | `MinimumCalls`, `SlowCallDuration`, `SlowCallRateThreshold` |

A condition is off when its threshold has no value.
At least one condition must be on.

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
Report(generation, calls, failures, slowCalls)
```

The store handles a report in one atomic operation.
The [store contract](../stores/README.md) defines this operation for all adapters.

1. Compare the generation of the report with the stored generation. If they are different, or the state is not `CLOSED`, ignore the counts.
2. Add the counts to the current bucket of the shared window. The store time selects the bucket.
3. Remove the buckets that are older than the window size.
4. Calculate the totals and evaluate the failure condition.
5. If the condition is true, make transition T1.
6. Return the authoritative state and the store time.

### Time of a report

A report goes into the bucket of the time at which the store receives it.
The error in time is at most one sync interval.
For this reason, the bucket duration must be equal to or longer than the sync interval.
The configuration validation checks this rule.

### Loss of a report

A report can fail, for example with a timeout.
The instance then does not know if the store added the counts.
The instance does not send the same counts again.
A lost report makes the shared window smaller than the truth for a short time.
A report that is counted two times can open a circuit for a healthy dependency. That error is worse.

The local window still contains the counts.
If the fault continues, the instance changes to local mode and uses the local window.

## 6. Configuration differences between instances

The failure condition uses the thresholds of the instance that sends the report.
During a rolling deployment, instances can have different thresholds for a short time.
The state contains a fingerprint of the configuration of the last writer.
When an instance sees a different fingerprint, it logs a warning and increments a metric.
The breaker continues to operate.

## 7. Limits of the design

- The shared window is the sum of reports, not an exact record of each call. Its error in time is at most one sync interval.
- Between the time at which the shared window reaches the threshold and the next report, the circuit is still `CLOSED`. A recorded failure causes an early report to keep this time short.
- An instance that cannot reach the store does not add to the shared window. It protects the dependency with its local window.
