# Operations

This document is for the engineers who run applications that use Faultvane.
It describes the implemented behavior. [Benchmarks](../benchmarks.md) gives the measured values.

## 1. Deployment patterns

| Pattern | Namespace | Result |
|---|---|---|
| One service, many instances | The environment, for example `production` | All instances of the service share the breaker of each dependency. |
| Many services call the same dependency, and they must share the breaker | The same namespace and the same breaker name in all services | One breaker for the dependency across services. All services must use the same thresholds. |
| Many services call the same dependency, and each service has its own breaker | A namespace for each service, for example `production.checkout` | Separate breakers. An error in one service does not open the circuit of the others. |
| Many regions | A namespace for each region | A regional outage opens only the regional circuit. |

Use separate breakers for each service unless you have a clear reason to share.
Services often have different timeouts and different traffic. One shared failure condition then fits none of them.

## 2. Store

Select the store that your application already operates.
[Stores](../stores/README.md) compares the adapters.

- The request path does not call the store in the `CLOSED` state. A slower store gives slower propagation, not slower requests. Only a probe request and the result of a probe call the store from the request path.
- Put the store near the application. The store timeout default is 100 ms.

### Redis

- A small Redis is sufficient. Each breaker uses three small keys.
- The breaker keys can share a Redis with other data. Then examine the eviction policy. See [Redis adapter](../stores/redis.md).
- Persistence is not necessary.

### Load estimate

```text
normal:   instances × breakers × (1 / SyncInterval)        operations each second
maximum:  instances × breakers × (1 / EarlyReportInterval) operations each second
```

With the defaults, 100 instances and 5 breakers give 1000 operations each second in normal operation.
The request rate of the application does not change this number.
The system benchmark measured approximately 2 store operations each second for each instance with one breaker. [Benchmarks](../benchmarks.md) gives the results.

## 3. Rollout of a new application version

- No release exists yet, so no two released versions exist. After version 1.0, the data layout of each adapter is a compatibility commitment.
- The Redis adapter keeps a schema version in the state hash. The SQL adapter and the MongoDB adapter keep no schema version.
- During a rolling deployment, instances can have different thresholds. The library reports this difference: the metric `config_mismatch` is `1`, and the instance writes a `WARN` log. The breaker continues to operate. The condition stops 10 store responses after the last write of an instance with a different configuration.
- A change of the library version that changes the format of the configuration fingerprint also gives this report during the deployment.
- To change a threshold safely, deploy the new value to all instances. No change in the store is necessary.

## 4. Shutdown

Call `breaker.Close(ctx)` for each breaker with a store when the application stops.
`Close` stops the synchronizer goroutine and sends the last report. The store timeout and the context limit this operation.
The adapters have no `Close` method. The application closes the client of the store.
A probe lease of a stopped instance ends at its end time.

## 5. Failure modes

| Symptom | Probable cause | Action |
|---|---|---|
| The circuit is `OPEN`, and the dependency is healthy | The thresholds are too low, or the classifier counts business errors as failures | Examine the failure rate metric. Change the classifier. |
| The circuit does not open during an outage | `MinimumCalls` is higher than the traffic in the window, or errors are classified as success | Lower `MinimumCalls` or set `FailureThreshold`. |
| The circuit flaps between `OPEN` and `CLOSED` | The dependency is partly healthy. One probe success closes the circuit too early | Raise `HalfOpenSuccessThreshold` and `HalfOpenMaxProbes`. Raise `OpenDuration`. |
| The circuit stays `HALF_OPEN` | Probes get no result, for example the calls hang | Add a timeout policy. Make `ProbeTimeout` longer than the timeout. |
| Instances are in local mode | The store is not reachable or slow | Examine the metrics `store_errors` and `store.duration`. Examine the network and the store. |
| Instances show different states | Some instances are in local mode | Examine the `mode` metric of each instance. |
| The circuit opens at different thresholds | Instances have different configurations | Examine the metric `config_mismatch` and the `WARN` log for a configuration difference. Compare `ConfigFingerprint()` of the instances. Make the configuration equal. |
| `config_mismatch` changes between `0` and `1` many times | A group of instances with a different configuration writes to the store less than one time in 10 responses | Make the configuration equal. |

## 6. Limits to know

- An instance observes a transition at most one sync interval after it occurs. During this time, calls can still reach an unhealthy dependency.
- In local mode, instances are not coordinated. Each instance has its own probe.
- After a store failover that loses data, the circuit state can go back to an older authoritative state. The breaker then detects the failure again.
- The library protects calls that go through a breaker. It cannot protect a dependency from callers that do not use it.
