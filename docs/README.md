# Faultvane documentation

## Plan

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the library must do. Each requirement and acceptance test has an identifier. |
| [Roadmap](roadmap.md) | The milestones, their exit criteria, and the definition of done. |
| [Decision records](adr/README.md) | The important design decisions, the alternatives, and the consequences. |

## Design

| Document | Content |
|---|---|
| [Architecture](architecture.md) | Components, request path, synchronization, module layout. |
| [API](api.md) | The planned public Go API and the error model. |
| [Configuration](configuration.md) | All options, their defaults, and the validation rules. |
| [Policy composition](policy-composition.md) | The order of the resilience policies and the interaction with retries. |
| [Observability](observability.md) | Metrics, logs, and traces. |

## Circuit breaker

| Document | Content |
|---|---|
| [State machine](circuit-breaker/state-machine.md) | States, transitions, and generation rules. |
| [Failure detection](circuit-breaker/failure-detection.md) | The sliding window and the shared aggregation. |
| [Redis data model](circuit-breaker/redis-data-model.md) | Keys, fields, atomic scripts, and TTL. |
| [Half-open coordination](circuit-breaker/half-open-coordination.md) | Probe permits and leases across instances. |
| [Degraded mode](circuit-breaker/degraded-mode.md) | Behavior without Redis, reconciliation, and the consistency model. |

## Quality and operation

| Document | Content |
|---|---|
| [Testing](testing.md) | Test levels, acceptance tests, chaos tests, and benchmarks. |
| [Operations](operations.md) | Deployment, capacity, failure modes, and troubleshooting. |

## Glossary

| Term | Meaning |
|---|---|
| Breaker | One circuit breaker with one name. It protects the calls to one dependency. |
| Dependency | The remote system that the application calls through a breaker. |
| Instance | One running process of an application that uses the library. |
| Namespace | The scope in which a breaker name is unique, for example `production`. Instances with the same namespace and breaker name share one state. |
| Store | The component that keeps the shared state. The Redis store is the first implementation. |
| Authoritative state | The breaker state in the store. It is the source of truth. |
| Snapshot | The copy of the authoritative state in the memory of an instance. |
| Generation | A number that increases by one at each state transition. |
| Window | The record of the recent calls that the breaker uses to detect failure. |
| Report | The batch of call counts that an instance sends to the store. |
| Probe | A call that the breaker permits in the `HALF_OPEN` state to test the dependency. |
| Lease | A probe permit with an expiry time. |
| Distributed mode | The normal mode. The instance synchronizes with the store. |
| Local mode | The degraded mode. The instance cannot reach the store and uses a local breaker. |
| Reconciliation | The procedure that returns an instance from local mode to distributed mode. |
| Request path | The code that runs in the goroutine of an application request. |
