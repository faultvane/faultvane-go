# Circuit breaker requirements

This document lists what the circuit breaker must do.
The [toolkit requirements](../requirements.md) apply to it also.
Each requirement has an identifier.
The [roadmap](../roadmap.md) and the tests refer to these identifiers.

The scope of this document is the Go library.
The requirements do not depend on the type of the shared store.

## 1. Problem

An in-process circuit breaker keeps a separate state in each instance.
When 10 instances call one dependency, each instance sees only a part of the failures.
The dependency can be fully down, and no instance reaches its failure threshold.

The results are:

- The circuit opens late.
- The unhealthy dependency continues to receive requests.
- The instances detect the recovery at different times.

The library shares the breaker state between instances through a shared store.
The store can be Redis, a SQL database, or another system with shared state.
The store must not become a single point of failure.

## 2. Goals

1. Prevent cascading failures.
2. Protect an unhealthy dependency from traffic.
3. Keep the shared state correct.
4. Prevent a stale state from overwriting a newer state.
5. Recover automatically.
6. Add very low latency to the request path.

The order of this list is the priority order.
The library does not trade the correctness of the shared state for more availability.

## 3. Functional requirements

### State machine

| ID | Requirement |
|---|---|
| SM-1 | The breaker has the states `CLOSED`, `OPEN`, and `HALF_OPEN`. |
| SM-2 | In `CLOSED`, the breaker permits calls and records their results. When the failure condition is true, the state becomes `OPEN`. |
| SM-3 | In `OPEN`, the breaker rejects calls immediately. It does not call the dependency. |
| SM-4 | When the open duration ends, the state becomes `HALF_OPEN`. |
| SM-5 | In `HALF_OPEN`, the breaker permits a limited number of probes. The limit is configurable. The default is 1. |
| SM-6 | A successful probe sequence changes the state to `CLOSED`. A failed probe changes the state to `OPEN`. |
| SM-7 | The breaker commits only valid transitions. It rejects all other transitions. |
| SM-8 | The state machine is deterministic. Its tests do not need the store. |

### Shared state

| ID | Requirement |
|---|---|
| DS-1 | Instances with the same namespace and breaker name share one state. |
| DS-2 | The shared state contains the circuit state, the transition time, the open duration, the generation, the data for the failure condition, and the probe leases. |
| DS-3 | The store holds the authoritative state. |
| DS-4 | The state schema causes minimal store load. |
| DS-5 | The breaker uses the store only through one interface. An adapter implements the interface for one type of store. |
| DS-6 | The breaker behavior is the same with each adapter. One shared conformance test suite checks each adapter. |
| DS-7 | The core does not depend on the client library of a store. Each adapter with an external dependency is a separate module. |
| DS-8 | An application can supply its own adapter. The documentation defines the contract of an adapter. |
| DS-9 | The shared state contains the configuration fingerprint of the instance that made the last accepted write. A shared state without a fingerprint is valid. |
| DS-10 | A store adapter can optionally send a notification for each transition. An instance that gets a notification synchronizes with the store and does not wait for the sync interval. An adapter without this function works as before. |
| DS-11 | A notification is only a signal to synchronize. The instance never changes its state from the content of a notification. A notification that is lost, late, repeated, or false changes no state and no permit decision. The sync interval stays the upper limit of the propagation time. |

### Generation

| ID | Requirement |
|---|---|
| GEN-1 | Each authoritative state has a generation. The generation increases at each transition and never decreases. |
| GEN-2 | Each state change in the store is atomic. |
| GEN-3 | The store rejects a write from an instance that holds an old generation. |

### Failure detection

| ID | Requirement |
|---|---|
| FD-1 | The failure condition supports a failure count threshold. |
| FD-2 | The failure condition supports a failure rate threshold with a minimum number of calls. |
| FD-3 | The breaker measures failures in a sliding window. |
| FD-4 | The failure condition optionally supports a slow call duration and a slow call rate. |
| FD-5 | The application changes these values through configuration only. |
| FD-6 | The circuit opens when the instances together reach the threshold. One instance does not have to reach it alone. |
| FD-7 | The library does not send each call result to the store as a separate operation. |
| FD-8 | An instance detects that the last writer of the shared state has a different configuration for the same breaker. The detection changes no permit decision and adds no cost to a call. |
| FD-9 | When a report fails in distributed mode, the instance keeps the counts of that report and sends them with the next report. The fault threshold limits the number of reports that fail in sequence. The instance discards its pending counts when it adopts a different generation. |

### Half-open coordination

| ID | Requirement |
|---|---|
| HO-1 | The probe limit applies to all instances together. |
| HO-2 | An instance gets a probe permit through an atomic operation. |
| HO-3 | A probe permit is a lease with an expiry time. A dead instance cannot hold a permit forever. |
| HO-4 | Two instances cannot hold the same permit. |
| HO-5 | An instance without a permit rejects the call. The caller can identify this rejection. |

### Degraded mode

| ID | Requirement |
|---|---|
| DG-1 | The instance detects that the store is not available. |
| DG-2 | The instance changes to local mode and continues to apply breaker behavior with local state. |
| DG-3 | No application request waits for the store without a limit. |
| DG-4 | The store latency is not part of the latency of a normal request. |
| DG-5 | Local mode has a configurable maximum duration. After that time, a configured policy applies. |
| DG-6 | Local states can be different between instances during local mode. The documentation states the guarantees for this period. |
| DG-7 | The behavior is defined and configurable when the store is not available and the instance has no local state. |
| DG-8 | An instance in local mode ignores notifications. Only the health check of local mode finds that the store is available again. |

### Recovery and reconciliation

| ID | Requirement |
|---|---|
| RC-1 | When the store is available again, the instance reads the authoritative state and compares generations. |
| RC-2 | The instance never writes its local state to the store without a generation check. |
| RC-3 | A newer authoritative state always wins against a local state. |
| RC-4 | After recovery, all instances converge to one state. The result is deterministic. |
| RC-5 | Recovery does not cause unnecessary state changes. |
| RC-6 | An instance that starts does not assume `CLOSED` before it reads the authoritative state or applies the configured start policy. |
| RC-7 | When the report of a reconciliation fails, the instance stays in local mode and keeps the counts of that report. The instance sends the counts again with the next reconciliation, with the same limits. FD-9 gives the rule for a failed report in distributed mode. |

### Store faults

| ID | Requirement |
|---|---|
| RF-1 | The library tolerates store restart, failover, connection failure, timeout, network partition, latency spikes, read failures, and write failures. |
| RF-2 | Each store operation has a strict timeout. |
| RF-3 | The library does not retry store operations without a limit. |
| RF-4 | A store fault does not use up the request goroutines of the application. |
| RF-5 | The library handles the loss of store data from eviction, restart, or failover. |
| RF-6 | A subscription to notifications that fails does not fail the creation of the breaker, and it fails and delays no call. A log and a statistic show the failure. A subscription works again after its connection to the store stopped. |

### Keys and TTL

| ID | Requirement |
|---|---|
| KEY-1 | The names of the records in the store are deterministic. |
| KEY-2 | The key design supports many services, environments, dependencies, and breakers without collisions. |
| KEY-3 | The name of the notification channel of a breaker is deterministic and obeys the rules of the record names. |
| TTL-1 | The state in the store has a TTL. Unused state does not stay forever. |
| TTL-2 | The TTL never deletes the state of a breaker that is in use. |

### Policy interaction

| ID | Requirement |
|---|---|
| PI-1 | The documentation defines the recommended order of retry, timeout, bulkhead, rate limiter, and load shedding with the breaker. |
| PI-2 | By default, the breaker counts one application request as one call. Internal retries do not count as separate calls. This is configurable. |
| PI-3 | Retries do not multiply the traffic to a dependency during an outage. |

### API and errors

| ID | Requirement |
|---|---|
| API-1 | The application runs a call through one simple function. It does not use details of the store. |
| API-2 | The API is safe for concurrent use by many goroutines. |
| API-3 | Each blocking function accepts a `context.Context` and obeys cancellation. |
| API-4 | The library starts no goroutine that it does not stop. |
| API-5 | The core does not depend on a framework. |
| ERR-1 | The library defines separate errors for an open circuit, a probe that is not permitted, a timeout, and an invalid configuration. |
| ERR-2 | The application can tell a dependency failure from a rejection by the breaker. |
| ERR-3 | A store fault is visible to the operator. It does not fail an application call. |

### Configuration

| ID | Requirement |
|---|---|
| CFG-1 | The library exposes the options that [configuration.md](configuration.md) lists. |
| CFG-2 | Each option has a safe default. |
| CFG-3 | An invalid configuration fails when the application creates the breaker. |
| CFG-4 | The breaker calculates a configuration fingerprint from the values that must be equal in all instances. A value that is local to one instance is not in the fingerprint. |

### Observability

| ID | Requirement |
|---|---|
| OB-1 | The library exposes the metrics that [observability.md](observability.md) lists. |
| OB-2 | Metric labels do not have high cardinality. |
| OB-3 | The library logs each state transition, each change of mode, each reconciliation, and each failed store operation. |
| OB-4 | Logs and traces contain no credentials, request payloads, or tokens. |
| OB-5 | OpenTelemetry tracing is optional. It adds no significant cost when it is off. |
| OB-6 | The library reports a configuration difference between instances with an event, a log, a statistic, and a metric. It writes the event and the log only when the difference starts and when it ends. The difference ends only after a fixed number of store responses in sequence without a different fingerprint. |
| OB-7 | The OpenTelemetry module exposes the number of transition notifications and a failed subscription as metrics. |

### Security

| ID | Requirement |
|---|---|
| SEC-1 | The store connection supports TLS and authentication. |
| SEC-2 | The library does not handle or log credentials. |

## 4. Performance requirements

| ID | Requirement |
|---|---|
| PF-1 | A call in the `CLOSED` state makes no store round trip. |
| PF-2 | The breaker adds less than 1 ms to a call in normal conditions. The internal target is less than 1 µs and zero allocations on the `CLOSED` path. |
| PF-3 | Store traffic depends on the number of instances and breakers. It does not depend on the number of application requests. |
| PF-4 | The library is stable under high concurrency. Lock contention is not a bottleneck. |
| PF-5 | Memory use has a fixed limit for each breaker. |
| PF-6 | The library leaks no goroutines. |
| PF-7 | Notifications do not cause a store load without a limit. An instance makes at most one synchronization in each early report interval, for each number of notifications. |

## 5. Acceptance tests

Each acceptance test is an automated test.
It runs against each store adapter of the repository, with a real instance of the store.

| ID | Scenario | Expected result |
|---|---|---|
| AT-1 | 10 instances share one dependency. The failure threshold is 5. The dependency becomes unavailable. | All instances observe `OPEN`. No instance has to see 5 failures alone. The traffic to the dependency stops. |
| AT-2 | 10 instances. The probe limit is 1. The open duration ends. | Exactly one instance gets the probe permit. A failed probe gives `OPEN`. A successful probe gives `CLOSED`. |
| AT-3 | The breaker is `CLOSED`. The store becomes unavailable. | Requests continue. The instance changes to local mode. No request waits for the store without a limit. The local breaker works. Metrics and logs show the fault. |
| AT-4 | The store was unavailable. The local state is different from the authoritative state. The store becomes available. | The instance reads the authoritative state and reconciles. A stale local state does not overwrite a newer state. Distributed mode continues. The state does not flap. |
| AT-5 | The store has generation 100. Instance A reads it. Instance B changes the state to generation 101. Instance A writes with generation 100. | The store rejects the write of instance A. The store stays at generation 101. |
| AT-6 | The store is unreachable for many instances. The instances get different local states. The store becomes available. | No stale local state overwrites the authoritative state. All instances converge to the same state. No split-brain condition stays. |
| AT-7 | Benchmarks with 1, 10, 50, and 100 instances, with the store healthy, unavailable, and recovering. | The results satisfy PF-1 to PF-6. The repository contains the results. |

## 6. Out of scope for the first release

- Store adapters other than the in-memory adapter and the Redis adapter. The SQL adapter is the next adapter in the [roadmap](../roadmap.md).
- A manual override that forces a breaker open or closed.
- A distributed rate limiter.
- Implementations in other languages.
