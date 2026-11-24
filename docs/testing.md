# Testing

## 1. Test levels

| Level | Subject | Tools | Needs Redis |
|---|---|---|---|
| Unit | State machine, window, configuration validation, classifier, local breaker | `testing`, fake clock | No |
| Property | Invariants of the state machine with random event sequences | Property-based testing | No |
| Concurrency | Request path with many goroutines, races between results and snapshot changes | `-race`, goroutine leak detector | No |
| Script | Each Lua script with exact inputs and outputs | Real Redis in a container | Yes |
| Integration | Many instances in one process against one Redis | Testcontainers | Yes |
| Chaos | Faults of Redis and of the dependency | Network fault proxy, container control | Yes |
| Benchmark | Request path cost and Redis load | `testing.B` | Both |

Tests that need libraries outside the standard library are in a separate `tests` module.
The core module keeps a `go.mod` with no dependencies.

## 2. Rules

- Unit tests use a fake clock. They do not sleep.
- Integration tests use short durations and wait for a condition with a deadline. They do not assert on a fixed sleep time.
- Each test creates its own namespace. Tests can run in parallel on one Redis.
- Tests assert on observable behavior: the result of `Execute`, the state, the number of calls that reach the dependency, and the content of Redis.
- A fake dependency counts the calls that it receives. Tests of protection assert on this count.
- A mock is permitted only at a real boundary. The store interface is the boundary for breaker tests.

## 3. Unit tests

| Area | Cases |
|---|---|
| `CLOSED` | Permits calls. Records results. Opens at the count threshold, at the rate threshold, and at the slow call threshold. Does not open below `MinimumCalls`. |
| `OPEN` | Rejects calls. Does not call the dependency. Stays `OPEN` before the open duration ends. |
| `HALF_OPEN` | Permits the probe limit. Rejects the other calls. Closes at the success threshold. Opens at one failure. |
| Window | Buckets rotate. Old buckets do not count. Totals are correct at bucket edges. |
| Classifier | Default rule. Custom rule. Cancellation by the caller is ignored. |
| Configuration | Each validation rule has a test for a valid value and for a value that is not valid. |
| Local mode | Entry from each state. Local thresholds. Time limit and each expired policy. Each start policy. |

## 4. Concurrency tests

| Case | Expected result |
|---|---|
| Many instances send a report that reaches the threshold at the same time. | One transition T1. The generation increases by 1. |
| Many instances request a probe at the same time. | The number of granted leases is equal to the probe limit. |
| An instance writes with an old generation. | Redis ignores the write. |
| A probe result arrives after its lease ended. | Redis ignores the result. |
| Many instances reconcile at the same time with different local states. | All instances end with the same state and generation. |
| Many goroutines call `Execute` while the snapshot changes. | No data race. Each call gets a result that is valid for one snapshot. |

## 5. Integration tests

- The state stays in Redis and a new instance reads it.
- Many instances share the state.
- Redis stops and starts again, with and without data loss.
- Reconciliation for each row of the table in [degraded mode](circuit-breaker/degraded-mode.md).
- TTL: a state in use does not expire, and an unused state expires.
- Redis Cluster: all keys of one breaker are in one slot.

## 6. Acceptance tests

The [requirements](requirements.md) define the acceptance tests AT-1 to AT-7.
Each acceptance test is one named test.
The roadmap states the milestone in which each test must pass.

## 7. Chaos tests

| Fault | Method | Assertions |
|---|---|---|
| Redis unavailable | Stop the container | Requests continue. Local mode starts. No call waits longer than the store timeout. |
| Redis slow | Add latency in the proxy | The same as Redis unavailable. The latency of `CLOSED` calls does not change. |
| Redis restart | Restart the container without persistence | `restore` gives the highest authoritative generation. |
| Redis partition | Cut the connection of a part of the instances | The two groups work. After the partition, all instances converge. |
| Dependency unavailable | The fake dependency fails | The circuit opens for all instances. The traffic stops. |
| Dependency recovery | The fake dependency succeeds again | One probe. The circuit closes for all instances. |
| Application restart | Create a new instance during `OPEN` | The new instance reads `OPEN`. It sends no call. |

## 8. Benchmarks

### Request path

| Benchmark | Target |
|---|---|
| `Execute` in `CLOSED`, one goroutine | Less than 100 ns, 0 allocations |
| `Execute` in `CLOSED`, parallel on all cores | No drop of throughput from contention |
| `Execute` in `OPEN` | Less than 50 ns, 0 allocations |

These values are targets, not measurements.
The first measurement becomes the baseline in the repository.
A later change must not make the baseline worse without a documented reason.

### System

The benchmark runs 1, 10, 50, and 100 instances in one process against one Redis.
It runs with Redis healthy, unavailable, and recovering.
It records:

- The latency that the breaker adds to a call, at percentiles 50, 99, and 99.9.
- The number of Redis operations each second, for different request rates.
- The memory for each breaker and the number of goroutines.

The run passes when the Redis operation rate does not change with the request rate, and when memory and goroutines stay constant during the run.

## 9. Continuous integration

Each change runs: build, `go vet`, static analysis, unit tests with `-race`, script tests, and integration tests.
Chaos tests and system benchmarks run on a schedule and before a release.
