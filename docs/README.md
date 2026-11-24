# Faultvane documentation

## Toolkit

| Document | Content |
|---|---|
| [Requirements](requirements.md) | The scope of the toolkit, the requirements for all policies, and the quality requirements. |
| [Architecture](architecture.md) | The layers, the policy model, shared state, and the module layout. |
| [Policy composition](policy-composition.md) | The order of the policies and the interaction with retries. |
| [Testing](testing.md) | The test levels, the coverage rule, and the quality gates. |
| [Roadmap](roadmap.md) | The milestones, their exit criteria, and the definition of done. |
| [Decision records](adr/README.md) | The important design decisions, the alternatives, and the consequences. |

## Policies

| Policy | Documents | Status |
|---|---|---|
| Circuit breaker | [Design documents](circuit-breaker/README.md) | Design complete |
| Timeout | Not written | Planned |
| Retry | Not written | Planned |
| Bulkhead | Not written | Planned |
| Concurrency limiter | Not written | Planned |
| Rate limiter | Not written | Planned |
| Load shedding | Not written | Planned |

Each policy gets its own directory with requirements and design documents before its implementation starts.

## Shared state

| Document | Content |
|---|---|
| [Stores](stores/README.md) | The store interface, the adapters, the contract, and the conformance tests. |
| [Redis adapter](stores/redis.md) | Keys, fields, atomic scripts, and TTL. |

## Glossary

| Term | Meaning |
|---|---|
| Policy | One resilience mechanism, for example a circuit breaker or a retry. |
| Pipeline | A sequence of policies that the library applies to one call. |
| Call | One run of the function that the application gives to a policy. |
| Rejection | The decision of a policy not to run a call. |
| Dependency | The remote system that the application calls through a policy. |
| Instance | One running process of an application that uses the library. |
| Store | The system that keeps shared state, for example Redis or a SQL database. |
| Adapter | The implementation of a store interface for one type of store. |
| Contract | The rules that each adapter must satisfy. |
| Request path | The code that runs in the goroutine of an application request. |
| Breaker | One circuit breaker with one name. It protects the calls to one dependency. |
| Namespace | The scope in which a name is unique, for example `production`. Instances with the same namespace and name share one state. |
| Authoritative state | The state in the store. It is the source of truth. |
| Snapshot | The copy of the authoritative state in the memory of an instance. |
| Generation | A number that increases by one at each state transition of a breaker. |
| Window | The record of the recent calls that a breaker uses to detect failure. |
| Report | The batch of call counts that an instance sends to the store. |
| Probe | A call that a breaker permits in the `HALF_OPEN` state to test the dependency. |
| Lease | A probe permit with an expiry time. |
| Distributed mode | The normal mode of a policy with a store. The instance synchronizes with the store. |
| Local mode | The degraded mode. The instance cannot reach the store and uses local state. |
| Reconciliation | The procedure that returns an instance from local mode to distributed mode. |
