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

A full run takes about 70 seconds after the images exist.
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
| Load (AT-7) | Each pod makes 2 million calls through a closed breaker with the default sync interval. | All calls succeed. The 99th percentile of each pod is below 1 ms. Each pod makes at most 6 store operations each second. No pod changes to local mode. |

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
| Each policy alone under load | Each pod makes 1 million calls through one policy. | All calls succeed. The 99th percentile is below 1 ms. |

### Shared rate limit

All 10 pods use a rate limiter with the Redis rate limit store and one name. Thus they share one bucket.

| Setting | Value |
|---|---|
| Rate | 500 calls each second |
| Burst | 50 |
| Local rate of each pod | 20 calls each second |
| Local burst of each pod | 2. This is `50 × 20 / 500`. |
| Load | Each worker of each pod makes one call, waits 1 ms, and makes the next call, for 3 seconds. All pods together make more than 10 times the rate. |

The limits come from the guarantee in the [rate limiter guide](ratelimit/README.md), section 5:
in a window of `t` seconds, all pods together permit at most `burst + rate × t + H` calls.
`H` is the number of tokens that the pods hold at the start of the window.
The test reads the start and the end of a window from the Redis clock, with the Redis command `TIME`.
The test counts the calls at the dependency.

| Scenario | Steps | Expected result |
|---|---|---|
| Ten pods share one rate (RL-39, RL-26) | The test reads the Redis time. Then it configures the pods, and all pods send the load. | The window starts before the first limiter starts, thus `H` is 0. The dependency gets at most `50 + 500 × t` calls and at least `0.7 × 500 × t` calls. The count of the dependency is equal to the number of permitted calls. Each other call gets the rate limit rejection. No call fails. All pods are in distributed mode after the load. |
| Redis outage (RL-40, RL-27, RL-28) | All pods are in distributed mode. The test stops the Redis container and waits until all pods are in local mode. Then all pods send the load. | The dependency gets from each pod at most `2 + 20 × d` calls and at least `0.7 × 20 × d` calls. `d` is the time of the load in that pod, in seconds, from the clock of the pod. The count of the dependency for each pod is equal to the permitted calls of that pod. Each other call gets the rate limit rejection. No call fails and no call ends with a timeout. Each pod is in local mode and has 3 or more store errors. |
| Redis recovery (RL-29, RL-26) | The test starts the Redis container. It waits until all pods are in distributed mode and each pod holds at most 1 token. It reads the Redis time and the tokens that the pods hold. Then all pods send the load. | `H` is the sum of the tokens that the pods hold. It is at most 10. The dependency gets at most `50 + 500 × t + H` calls and at least `0.7 × 500 × t` calls. No call fails. All pods are in distributed mode after the load. |

The outage and the recovery are one test.

The lower limit of 70% is the limit of the requirement RL-39. It shows that the pods use the limit and do not only stay below it.

Measured values of 8 runs on the machine of section 5:

| Part | Window | Calls at the dependency | Permitted range for a window of 3.13 s |
|---|---|---|---|
| Ten pods share one rate | 3.13 s to 3.16 s | 1509 to 1518 | 1095 to 1615 |
| Redis outage, each pod | 3.00 s | 61 | 42 to 62 for a window of 3.00 s |
| Redis recovery, `H` = 10 | 3.13 s to 3.14 s | 1475 to 1513 | 1095 to 1625 |

## 5. Measured results

Machine: Apple M4, 10 cores, Docker Desktop, 2026-11-24. 10 pods, 2 workers in each pod, a call that does nothing.
The number of workers in each pod is two times the number of processor cores divided by the number of pods, and at least 1.
The breaker uses the default sync interval of 500 ms in the load scenario.
The percentiles are the values of the slowest pod. They include the two clock reads of the measurement.

| Policy | Store | Calls each second, all pods | p50 | p99 | p99.9 | Store operations each second, each pod |
|---|---|---|---|---|---|---|
| Circuit breaker | Redis | 26.7 million | 0.17 µs | 0.67 µs | 3.12 µs | 2.7 |
| Circuit breaker | PostgreSQL | 29.6 million | 0.17 µs | 0.58 µs | 2.92 µs | 3.0 |
| Circuit breaker | MySQL | 29.6 million | 0.17 µs | 0.62 µs | 2.88 µs | 3.0 |
| Circuit breaker | MongoDB | 30.4 million | 0.17 µs | 0.54 µs | 2.92 µs | 3.0 |
| Circuit breaker | None | 27.6 million | 0.17 µs | 0.46 µs | 2.92 µs | 0 |
| Bulkhead | None | 44.6 million | 0.04 µs | 0.38 µs | 1.75 µs | 0 |
| Concurrency limiter | None | 32.8 million | 0.12 µs | 0.42 µs | 2.88 µs | 0 |
| Rate limiter | None | 37.3 million | 0.08 µs | 0.38 µs | 2.83 µs | 0 |
| Load shedding | None | 47.2 million | 0.04 µs | 0.25 µs | 2.12 µs | 0 |
| Retry | None | 52.5 million | 0.04 µs | 0.12 µs | 1.92 µs | 0 |
| Timeout | None | 14.1 million | 0.33 µs | 2.54 µs | 7.88 µs | 0 |

What the results show:

- The type of the store does not change the cost of a call. The request path does not use the store.
- Each pod makes about 3 store operations each second at more than 2.5 million calls each second. The run is shorter than one second, so the first read and the last report increase this value. The store load does not follow the request rate.
- No pod changes to local mode during the load.
- All 10 pods share the 10 cores of one machine. The numbers show that the policies are not a bottleneck. They are not a capacity value for a production server.

On a GitHub runner with 4 cores and 1 worker in each pod, the same scenario gave about 11 million calls each second for all pods and 2.3 store operations each second for each pod.

## 6. Defects that the scenarios found

| Defect | Correction |
|---|---|
| The rate limiter rejected a few calls with a full bucket under parallel load. A goroutine read the clock, then a different goroutine refilled the bucket with a later time. The first goroutine then calculated a negative refill. | The limiter now uses 0 for a negative elapsed time. A regression test covers it. |
| On a runner with 4 cores, 40 workers used all processor time. Store operations of the pods then took longer than a store timeout of 80 ms, and a pod changed to local mode. The breaker behaved correctly, but the scenario was not realistic. | The load scenario now uses the default sync interval and a number of workers that fits the machine. It also fails when a pod changes to local mode. |
| With MongoDB, the first reports of all pods timed out, because the first write created the collection. At that time, the breaker did not send a failed report again, so the circuit did not open. | The pod now calls `EnsureIndexes` at setup, as the adapter document says. The adapter document states that this call is necessary before traffic. The breaker now also sends the counts of a failed report with the next report (FD-9, [ADR 0010](adr/0010-resend-failed-reports.md)). |

## 7. Limits

- All containers run on one machine. The network has almost no latency and no loss.
- The shared rate limit scenarios use one Redis server without cluster mode. The tests of the Redis adapter use a server with cluster mode.
- A store outage is a stopped container. The scenarios do not test a slow store or a partial partition here. The Redis adapter tests cover a slow store with a proxy.
- The scenarios module is test equipment. The coverage rule does not apply to it, as for the example programs.
