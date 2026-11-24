# Observability

## 1. Principles

- The core module depends only on the standard library. It defines an observer interface and uses `log/slog`.
- A separate module supplies an OpenTelemetry observer for metrics and traces.
- The request path updates counters only. It does not call the observer and does not write logs for each call.
- No signal contains credentials, request payloads, or tokens.

## 2. Metrics

All metric names start with `faultvane_circuit_breaker_`.

| Metric | Type | Extra labels | Meaning |
|---|---|---|---|
| `state` | Gauge | `state` | `1` for the current state, `0` for the other states. |
| `calls_total` | Counter | `result` | Calls that the breaker permitted. `result` is `success`, `failure`, or `ignored`. |
| `slow_calls_total` | Counter | | Calls that were slow. |
| `rejected_calls_total` | Counter | `reason` | Calls that the breaker rejected. `reason` is `open` or `probe_not_permitted`. |
| `transitions_total` | Counter | `from`, `to` | State transitions that this instance observed. |
| `probes_total` | Counter | `result` | Probe results. `result` is `success` or `failure`. |
| `store_errors_total` | Counter | `operation` | Store operations that failed. |
| `store_latency_seconds` | Histogram | `operation` | Duration of store operations. |
| `mode` | Gauge | `mode` | `1` for the current mode: `distributed` or `local`. |
| `local_mode_total` | Counter | | Number of changes to local mode. |
| `local_mode_expired` | Gauge | | `1` when local mode is longer than the maximum time. |
| `reconciliations_total` | Counter | `result` | Reconciliations. `result` is `adopted`, `reported`, or `restored`. |
| `stale_writes_total` | Counter | `operation` | Writes that Redis ignored because of an old generation. |
| `config_mismatch_total` | Counter | | Reports that found a different configuration fingerprint. |

The metrics of the requirements map to these names:

| Requirement name | Metric |
|---|---|
| `circuit_breaker_state` | `state` |
| `circuit_breaker_calls`, `_failures`, `_successes` | `calls_total` with the `result` label |
| `circuit_breaker_rejected_calls` | `rejected_calls_total` |
| `circuit_breaker_opened_total`, `_closed_total`, `_half_open_total` | `transitions_total` with the `to` label |
| `circuit_breaker_probe_success_total`, `_probe_failure_total` | `probes_total` with the `result` label |
| `circuit_breaker_redis_errors` | `store_errors_total` |
| `circuit_breaker_redis_latency` | `store_latency_seconds` |
| `circuit_breaker_local_mode_total` | `local_mode_total` |
| `circuit_breaker_reconciliation_total` | `reconciliations_total` |

### Labels

Each metric has the labels `namespace` and `name`.
The validation limits the characters and the length of both.
The number of breakers in an application is small and fixed.

The library adds no label for an instance, a request, a user, or an error message.
The metrics system of the application adds the instance label.

### Recommended alerts

| Condition | Meaning |
|---|---|
| `state{state="open"} == 1` for longer than some minutes | The dependency is down. |
| `mode{mode="local"} == 1` | The instance cannot reach Redis. |
| `local_mode_expired == 1` | Redis is down for longer than the maximum time. |
| `transitions_total` increases quickly | The circuit flaps. Examine the thresholds. |
| `config_mismatch_total` increases outside a deployment | Instances have different configurations. |

## 3. Logs

The library writes structured logs with `log/slog`.

| Event | Level | Fields |
|---|---|---|
| Transition `CLOSED → OPEN` | `WARN` | namespace, name, generation, failures, calls, failure rate |
| Transition `OPEN → HALF_OPEN` | `INFO` | namespace, name, generation |
| Transition `HALF_OPEN → CLOSED` | `INFO` | namespace, name, generation |
| Transition `HALF_OPEN → OPEN` | `WARN` | namespace, name, generation |
| Change to local mode | `WARN` | namespace, name, last generation, cause |
| Local mode is longer than the maximum time | `ERROR` | namespace, name, duration, policy |
| Store reachable again | `INFO` | namespace, name, duration of local mode |
| Reconciliation | `INFO` | namespace, name, local state, authoritative state, generations, result |
| A write that Redis ignored | `DEBUG` | namespace, name, operation, generations |
| Probe lease or result operation failed | `WARN` | namespace, name, generation, cause |
| Configuration mismatch | `WARN` | namespace, name, the two fingerprints |

Rules:

- An instance logs a transition one time, when its snapshot changes.
- Repeated store faults are limited: one log at the change to local mode, not one log for each fault.
- The `cause` field contains the type of the error, not data of a request.

## 4. Traces

The OpenTelemetry module adds attributes to the active span of the call.
It creates a child span only for a store operation on the request path, that is, for a probe.

| Attribute | Value |
|---|---|
| `faultvane.breaker.name` | The name of the breaker |
| `faultvane.breaker.state` | The state at the permit decision |
| `faultvane.breaker.mode` | `distributed` or `local` |
| `faultvane.breaker.decision` | `permitted`, `rejected`, or `probe` |
| `faultvane.breaker.rejection_reason` | `open` or `probe_not_permitted` |
| `faultvane.breaker.generation` | The generation of the permit |

Tracing is off when the application does not install the observer.
In that condition, the request path contains no tracing code.

## 5. Observer interface

The observer receives events, not single calls:

- State transition.
- Change of mode.
- Reconciliation result.
- Store fault.
- Configuration mismatch.

The metrics observer reads the call counters at collection time.
This keeps the request path free of interface calls.
