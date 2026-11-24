# Scenarios in Docker

The scenarios test Faultvane as a real deployment uses it: many separate processes, a real network, and real stores.
Each scenario starts from a clean state and has an exact expected result.

## 1. What runs

One Docker Compose project, `scenarios/docker-compose.yml`:

| Container | Count | Role |
|---|---|---|
| `pod` | 10 | One application instance. It runs the policies in front of calls to the dependency. |
| `dependency` | 1 | A service that the test controls: healthy, failing, or slow. It counts the calls of each pod and the calls in progress. |
| `redis`, `postgres`, `mysql`, `mongodb` | 1 each | The shared stores. |

The test `scenarios/scenario_test.go` runs on the host.
It builds the two programs, starts the containers, sends a configuration to each pod, sends load, and reads the results.
It stops and starts store containers to make real outages.

## 2. Run the scenarios

You need Docker and Go.

```sh
sh scripts/scenarios.sh
```

A full run takes about one minute after the images exist.
Set `FAULTVANE_SCENARIOS_KEEP=1` to keep the containers after the run.

The workflow `.github/workflows/scenarios.yml` runs the scenarios each day.

## 3. Circuit breaker scenarios

Each scenario runs four times: with Redis, PostgreSQL, MySQL, and MongoDB.

| Scenario | Steps | Expected result |
|---|---|---|
| Ten pods, one failure each (AT-1) | The dependency fails. Each pod makes one call. The failure threshold is 5. | All 10 pods are open. Each pod has exactly 1 failure. 200 more calls are all rejected. The dependency got exactly 10 calls. |
| Ten pods report at the same time (GEN-1, GEN-3) | The dependency fails. All pods make 300 calls with 10 workers at the same time. | Each pod observes exactly one open transition and no other transition. The dependency gets only a part of the 3000 calls. |
| One probe from ten pods (AT-2) | The circuit is open. The open duration ends. The dependency is healthy and slow. Each pod sends 3 calls at the same time. | Exactly 1 call reaches the dependency. 29 calls get the probe rejection. Then all pods are closed. |
| Loss of data in the store (RF-5) | The circuit is open. The test deletes the state in the store. | A pod that starts after the loss reads the open state. The other pods restored it. |
| Pod restart (RC-6) | The circuit is open. The test restarts one pod container. | The pod reads open at its start. It rejects 5 calls. The dependency gets no call. |
| Store outage and recovery (AT-3, AT-4, AT-6) | The test stops the store container. All pods make calls. Then the dependency fails for 5 of the pods. Then the test starts the store. | Without the store, all 500 calls succeed and all pods are in local mode. The 5 pods open locally and the other 5 stay closed. After the store is back, all pods are in distributed mode with one state. |
| Load (AT-7) | Each pod makes 2 million calls with 4 workers through a closed breaker. | All calls succeed. The 99th percentile of each pod is below 1 ms. Each pod makes at most 15 store operations each second. |

## 4. Scenarios for the other policies

Each scenario runs in all 10 pods at the same time.

| Policy | Steps | Expected result |
|---|---|---|
| Bulkhead | Limit 5. The dependency is slow. 50 calls at the same time. | Exactly 5 calls run and 45 are rejected in each pod. The dependency never has more than 5 calls in progress from one pod. |
| Rate limiter | 200 calls each second, burst 20. 3000 calls. | The permitted calls are from 20 to `20 + 200 × duration`. All other calls are rejected. |
| Timeout | Limit 100 ms. The dependency takes 3 s. | All 10 calls end with the timeout error in less than 1.5 s. |
| Retry | 3 attempts, budget 0.1. The dependency fails. 500 requests. | Each pod makes from 550 to 561 attempts. The retries add about 10% traffic, not 200%. |
| Concurrency limiter | Start limit 20, latency threshold 50 ms. The dependency takes 200 ms. | The limit goes down to 3 or less. The dependency never has more than 20 calls in progress from one pod. |
| Load shedding | Maximum 10 in progress. The dependency is slow. 30 calls at the same time. | With low priority, exactly 5 run. With critical priority, exactly 10 run. |
| All policies together | Load shedding, rate limiter, concurrency limiter, bulkhead, circuit breaker with Redis, retry with a gate and a budget, timeout. The dependency fails, then recovers. | In the outage, all pods open and the dependency gets less than half of the 5000 requests as attempts. After the recovery, all pods close and 1000 calls all succeed. |
| Each policy alone under load | Each pod makes 1 million calls with 4 workers through one policy. | All calls succeed. The 99th percentile is below 1 ms. |

## 5. Measured results

Machine: Apple M4, 10 cores, Docker Desktop, 2026-11-24. 10 pods, 4 workers in each pod, a call that does nothing.
The percentiles are the values of the slowest pod. They include the two clock reads of the measurement.

| Policy | Store | Calls each second, all pods | p50 | p99 | p99.9 | Store operations each second, each pod |
|---|---|---|---|---|---|---|
| Circuit breaker | Redis | 24.9 million | 0.17 µs | 0.88 µs | 4.62 µs | 10.1 |
| Circuit breaker | PostgreSQL | 28.0 million | 0.17 µs | 0.62 µs | 3.04 µs | 9.1 |
| Circuit breaker | MySQL | 25.6 million | 0.17 µs | 0.75 µs | 6.75 µs | 8.3 |
| Circuit breaker | MongoDB | 28.0 million | 0.17 µs | 0.62 µs | 3.54 µs | 9.8 |
| Circuit breaker | None | 26.8 million | 0.12 µs | 0.50 µs | 4.17 µs | 0 |
| Bulkhead | None | 38.7 million | 0.08 µs | 0.58 µs | 2.88 µs | 0 |
| Concurrency limiter | None | 30.3 million | 0.12 µs | 0.54 µs | 2.96 µs | 0 |
| Rate limiter | None | 34.8 million | 0.12 µs | 0.58 µs | 2.96 µs | 0 |
| Load shedding | None | 35.6 million | 0.08 µs | 0.58 µs | 3.50 µs | 0 |
| Retry | None | 48.2 million | 0.04 µs | 0.17 µs | 2.79 µs | 0 |
| Timeout | None | 14.2 million | 0.38 µs | 2.58 µs | 11.04 µs | 0 |

What the results show:

- The type of the store does not change the cost of a call. The request path does not use the store.
- With a sync interval of 100 ms, each pod makes about 10 store operations each second at 2.5 million calls each second. The store load does not follow the request rate.
- All 10 pods share the 10 cores of one machine. The numbers show that the policies are not a bottleneck. They are not a capacity value for a production server.

## 6. Defects that the scenarios found

| Defect | Correction |
|---|---|
| The rate limiter rejected a few calls with a full bucket under parallel load. A goroutine read the clock, then a different goroutine refilled the bucket with a later time. The first goroutine then calculated a negative refill. | The limiter now uses 0 for a negative elapsed time. A regression test covers it. |
| With MongoDB, the first reports of all pods timed out, because the first write created the collection. The breaker does not send a failed report again, so the circuit did not open. | The pod now calls `EnsureIndexes` at setup, as the adapter document says. The adapter document states that this call is necessary before traffic. |

## 7. Limits

- All containers run on one machine. The network has almost no latency and no loss.
- A store outage is a stopped container. The scenarios do not test a slow store or a partial partition here. The Redis adapter tests cover a slow store with a proxy.
- The scenarios module is test equipment. The coverage rule does not apply to it, as for the example programs.
