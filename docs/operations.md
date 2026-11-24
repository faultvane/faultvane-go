# Operations

This document is for the engineers who run applications that use Faultvane.
It describes the planned behavior. The values get an update when measurements exist.

## 1. Deployment patterns

| Pattern | Namespace | Result |
|---|---|---|
| One service, many instances | The environment, for example `production` | All instances of the service share the breaker of each dependency. |
| Many services call the same dependency, and they must share the breaker | The same namespace and the same breaker name in all services | One breaker for the dependency across services. All services must use the same thresholds. |
| Many services call the same dependency, and each service has its own breaker | A namespace for each service, for example `production.checkout` | Separate breakers. An error in one service does not open the circuit of the others. |
| Many regions | A namespace for each region | A regional outage opens only the regional circuit. |

Use separate breakers for each service unless you have a clear reason to share.
Services often have different timeouts and different traffic. One shared failure condition then fits none of them.

## 2. Redis

- A small Redis is sufficient. Each breaker uses three small keys.
- The breaker keys can share a Redis with other data. Then examine the eviction policy. See [Redis data model](circuit-breaker/redis-data-model.md).
- Persistence is not necessary.
- Put Redis near the application. The store timeout default is 100 ms.

### Load estimate

```text
normal:   instances × breakers × (1 / SyncInterval)        operations each second
maximum:  instances × breakers × (1 / EarlyReportInterval) operations each second
```

With the defaults, 100 instances and 5 breakers give 1000 operations each second in normal operation.
The request rate of the application does not change this number.

## 3. Rollout of a new application version

- New and old library versions use the same key schema. The field `v` gives the schema version.
- During a rolling deployment, instances can have different thresholds. The library logs a configuration mismatch. This stops when the deployment ends.
- To change a threshold safely, deploy the new value to all instances. No Redis change is necessary.

## 4. Shutdown

Call `store.Close(ctx)` when the application stops.
The store sends the last report with the time limit of the context and stops its goroutine.
A probe lease of a stopped instance ends at its end time.

## 5. Failure modes

| Symptom | Probable cause | Action |
|---|---|---|
| The circuit is `OPEN`, and the dependency is healthy | The thresholds are too low, or the classifier counts business errors as failures | Examine the failure rate metric. Change the classifier. |
| The circuit does not open during an outage | `MinimumCalls` is higher than the traffic in the window, or errors are classified as success | Lower `MinimumCalls` or set `FailureThreshold`. |
| The circuit flaps between `OPEN` and `CLOSED` | The dependency is partly healthy. One probe success closes the circuit too early | Raise `HalfOpenSuccessThreshold` and `HalfOpenMaxProbes`. Raise `OpenDuration`. |
| The circuit stays `HALF_OPEN` | Probes get no result, for example the calls hang | Add a timeout policy. Make `ProbeTimeout` longer than the timeout. |
| Instances are in local mode | Redis is not reachable or slow | Examine `store_errors_total` and `store_latency_seconds`. Examine the network and Redis. |
| Instances show different states | Some instances are in local mode | Examine the `mode` metric of each instance. |
| `stale_writes_total` is high | Many instances report at a transition. This is normal for a short time | No action, unless it continues. |
| `config_mismatch_total` increases | Instances have different configurations | Make the configuration equal. |

## 6. Limits to know

- An instance observes a transition at most one sync interval after it occurs. During this time, calls can still reach an unhealthy dependency.
- In local mode, instances are not coordinated. Each instance has its own probe.
- After a Redis failover that loses data, the circuit state can go back to an older authoritative state. The breaker then detects the failure again.
- The library protects calls that go through a breaker. It cannot protect a dependency from callers that do not use it.
