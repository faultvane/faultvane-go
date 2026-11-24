# Observability

## 1. Principles

- The core module depends only on the standard library. The package `circuitbreaker` defines an observer interface, keeps counters, and writes logs with `log/slog`.
- A separate module, `otel`, exports the counters as OpenTelemetry metrics and adds attributes to traces. Its package name is `faultvaneotel`.
- The request path updates counters only. It does not call the observer and does not write logs for each call.
- The breaker puts no credentials, request payloads, or tokens in a signal.

| Signal | Source | Needs OpenTelemetry |
|---|---|---|
| Counters | `breaker.Stats()` | No |
| Logs | `log/slog`, with the logger of `WithLogger` | No |
| Events | `WithObserver` | No |
| Metrics | `faultvaneotel.RegisterMetrics` | Yes |
| Store duration | `faultvaneotel.InstrumentStore` | Yes |
| Trace attributes | `faultvaneotel.Traced` | Yes |

## 2. Metrics

### Setup

```go
registration, err := faultvaneotel.RegisterMetrics(meter, paymentBreaker, inventoryBreaker)
if err != nil {
    return err
}
defer registration.Unregister()
```

`RegisterMetrics` registers one callback for all breakers of the call.
The callback reads `Stats` of each breaker at collection time.
The request path contains no metric code.

### Names

All metric names start with `faultvane.circuit_breaker.`.

| Metric | Instrument | Extra attributes | Meaning |
|---|---|---|---|
| `state` | Observable gauge | `faultvane.breaker.state` | `1` for the current state, `0` for the other states. The values are `closed`, `open`, and `half_open`. |
| `mode` | Observable gauge | `faultvane.breaker.mode` | `1` for the current mode, `0` for the other mode. The values are `distributed` and `local`. |
| `local_mode_expired` | Observable gauge | | `1` when local mode is longer than the maximum time. |
| `calls` | Observable counter | `result` | Calls that the breaker permitted. `result` is `success`, `failure`, or `ignored`. |
| `slow_calls` | Observable counter | | Calls that were slow. |
| `rejected_calls` | Observable counter | `faultvane.breaker.rejection_reason` | Calls that the breaker rejected. The reason is `open` or `probe_not_permitted`. |
| `probes` | Observable counter | `result` | Probe results. `result` is `success` or `failure`. |
| `transitions` | Observable counter | `to` | State transitions that this instance observed. `to` is `open`, `half_open`, or `closed`. |
| `store_errors` | Observable counter | | Store operations that failed. |
| `local_mode` | Observable counter | | Number of changes to local mode. |
| `reconciliations` | Observable counter | | Reconciliations with the store. |
| `store.duration` | Histogram, unit `s` | `operation`, `error` | Duration of store operations. Only `InstrumentStore` records it. |

An exporter can change these names. For example, a Prometheus exporter usually replaces the dots and adds a suffix to a counter.

The library has no metric for a write that the store ignored and no metric for a configuration difference between instances.

### Attributes

Each metric of `RegisterMetrics` has the attribute `faultvane.breaker.name`.
The validation limits the characters and the length of the name.
The number of breakers in an application is small and fixed.

The metrics have no attribute for the namespace. The namespace is an option of the store adapter, and the breaker does not know it.
The library adds no attribute for an instance, a request, a user, or an error message.
The metrics system of the application adds the instance and the namespace.

### Store duration

```go
instrumented, err := faultvaneotel.InstrumentStore(store, meter)
if err != nil {
    return err
}
breaker, err := circuitbreaker.New("payment-service", circuitbreaker.WithStore(instrumented))
```

`InstrumentStore` returns a store that measures each operation and then calls the store that it holds.

| Attribute | Values |
|---|---|
| `operation` | `load`, `report`, `acquire_probe`, `complete_probe`, `restore` |
| `error` | `true` when the operation returned an error |

The histogram has no attribute for the name of the breaker.

### The same numbers without OpenTelemetry

`breaker.Stats()` returns the numbers that the metrics use.
An application with a different metrics system reads `Stats` at an interval and exports the fields.
[API](api.md) lists the fields.

### Names from the requirements

| Requirement name | Metric |
|---|---|
| `circuit_breaker_state` | `state` |
| `circuit_breaker_calls`, `_failures`, `_successes` | `calls` with the `result` attribute |
| `circuit_breaker_rejected_calls` | `rejected_calls` |
| `circuit_breaker_opened_total`, `_closed_total`, `_half_open_total` | `transitions` with the `to` attribute |
| `circuit_breaker_probe_success_total`, `_probe_failure_total` | `probes` with the `result` attribute |
| `circuit_breaker_redis_errors` | `store_errors` |
| `circuit_breaker_redis_latency` | `store.duration` |
| `circuit_breaker_local_mode_total` | `local_mode` |
| `circuit_breaker_reconciliation_total` | `reconciliations` |

### Recommended alerts

| Condition | Meaning |
|---|---|
| `state` with the state `open` is `1` for longer than some minutes | The dependency is down. |
| `mode` with the mode `local` is `1` | The instance cannot reach the store. |
| `local_mode_expired` is `1` | The store is down for longer than the maximum time. |
| `transitions` increases quickly | The circuit flaps. Examine the thresholds. |
| `store_errors` increases | The store or the network has a problem. |

## 3. Logs

The breaker writes structured logs with `log/slog`.
The default logger is `slog.Default()`. `WithLogger` sets a different logger.

| Event | Message | Level |
|---|---|---|
| State change to `OPEN` | `circuit breaker state changed` | `WARN` |
| State change to `HALF_OPEN` or `CLOSED` | `circuit breaker state changed` | `INFO` |
| Change to local mode | `circuit breaker mode changed` | `WARN` |
| Change to distributed mode | `circuit breaker mode changed` | `INFO` |
| Reconciliation | `circuit breaker reconciled with the store` | `INFO` |
| Store operation failed | `circuit breaker store operation failed` | `DEBUG` |
| Local mode is longer than the maximum time | `circuit breaker local mode is longer than the maximum time` | `ERROR` |
| The observer panicked | `circuit breaker observer panic` | `ERROR` |

Each log, except the log for an observer panic, has these attributes:

| Attribute | Content |
|---|---|
| `breaker` | The name of the breaker |
| `from` | The state before: `closed`, `open`, or `half_open` |
| `to` | The state after |
| `generation` | The generation after the event |
| `mode` | The mode after the event: `distributed` or `local` |
| `cause` | The text of the store error. Only a store fault and a change to local mode because of a fault have it. |

The log for an observer panic has the attributes `breaker` and `panic`.

Rules:

- An instance logs a state change one time, when its view of the state changes.
- A store fault outside local mode gives one `DEBUG` log. A failed health check in local mode gives no log. It only increases the counter of store errors.
- A log for a failed store operation does not set `from`, `to`, and `generation`. They show `closed` and `0`.
- The `cause` attribute contains the text of the error from the store adapter. It can contain the address of the store. It contains no data of an application call, because a store operation gets none.
- The logs do not contain the namespace, the counts of the window, or the failure rate.

## 4. Traces

```go
pipeline := faultvane.Combine(faultvaneotel.Traced(breaker), attempt)
```

`Traced` returns a policy that runs the call through the breaker.
When the context holds a span that records, the policy sets these attributes on that span after the call:

| Attribute | Value |
|---|---|
| `faultvane.breaker.name` | The name of the breaker |
| `faultvane.breaker.state` | The state immediately before the permit decision |
| `faultvane.breaker.mode` | `distributed` or `local` |
| `faultvane.breaker.decision` | `permitted`, `probe`, or `rejected` |
| `faultvane.breaker.rejection_reason` | `open` or `probe_not_permitted`. Only a rejected call has it. |

`Traced` creates no span. It has no attribute for the generation.
When the span does not record, `Traced` calls the breaker directly and sets nothing.
Tracing is off when the application does not use `Traced`.
In that condition, the request path contains no tracing code.

## 5. Observer interface

```go
type Observer interface {
    Observe(Event)
}
```

The observer receives events, not single calls:

| Kind of event | Text of `Kind.String()` | When |
|---|---|---|
| `StateChanged` | `state_changed` | The view of the instance changes to a different state. |
| `ModeChanged` | `mode_changed` | The instance changes to local mode or to distributed mode. |
| `Reconciled` | `reconciled` | A reconciliation with the store completed. |
| `StoreFault` | `store_fault` | A store operation failed outside a health check of local mode. |
| `LocalModeExpired` | `local_mode_expired` | Local mode is longer than the maximum time, or the local fallback is off. |

[API](api.md) lists the fields of an event and the rules for an observer.
The breaker writes the log of an event before it calls the observer.
No ready-made observer exists. The OpenTelemetry module does not use the observer. It reads `Stats`.
