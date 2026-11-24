# Circuit breaker

The circuit breaker stops calls to a dependency that fails.
It works as a local breaker in one process.
With a shared store, all instances of a service share one circuit.

| Document | Content |
|---|---|
| [Requirements](requirements.md) | What the breaker must do. Each requirement and acceptance test has an identifier. |
| [Architecture](architecture.md) | Components, request path, synchronization, time, and concurrency. |
| [State machine](state-machine.md) | States, transitions, and generation rules. |
| [Failure detection](failure-detection.md) | The sliding window and the shared aggregation. |
| [Half-open coordination](half-open-coordination.md) | Probe permits and leases across instances. |
| [Degraded mode](degraded-mode.md) | Behavior without the store, reconciliation, and the consistency model. |
| [API](api.md) | The planned public Go API and the error model. |
| [Configuration](configuration.md) | All options, their defaults, and the validation rules. |
| [Observability](observability.md) | Metrics, logs, and traces. |
| [Operations](operations.md) | Deployment, capacity, failure modes, and troubleshooting. |

The shared state is in a store. [Stores](../stores/README.md) describes the store interface and the adapters.
