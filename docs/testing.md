# Testing

## 1. Test levels

| Level | Subject | Tools | Needs a real store |
|---|---|---|---|
| Unit | State machine, window, configuration validation, classifier, local breaker | `testing`, fake clock | No |
| Property | Invariants of the state machine with random event sequences | Property-based testing | No |
| Concurrency | Request path with many goroutines, races between results and snapshot changes | `-race`, goroutine leak detector | No |
| Conformance | Each store adapter against the store contract | `store/storetest`, real store in a container | Yes, except in-memory |
| Integration | Many instances in one process against one store | In-memory adapter, Testcontainers | Both |
| Chaos | Faults of the store and of the dependency | Network fault proxy, container control | Yes |
| Benchmark | Request path cost and store load | `testing.B` | Both |

Tests that need libraries outside the standard library are in a separate `tests` module.
The core module keeps a `go.mod` with no dependencies.

## 2. Rules

- Unit tests use a fake clock. They do not sleep.
- Integration tests use short durations and wait for a condition with a deadline. They do not assert on a fixed sleep time.
- Each test creates its own namespace. Tests can run in parallel on one store.
- Tests assert on observable behavior: the result of `Execute`, the state, the number of calls that reach the dependency, and the state that the store returns.
- A fake dependency counts the calls that it receives. Tests of protection assert on this count.
- Tests of the core use the in-memory adapter. It is a real implementation of the contract, not a mock.
- A fake store that returns errors or waits is permitted for tests of store faults.

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
| An instance writes with an old generation. | The store ignores the write. |
| A probe result arrives after its lease ended. | The store ignores the result. |
| Many instances reconcile at the same time with different local states. | All instances end with the same state and generation. |
| Many goroutines call `Execute` while the snapshot changes. | No data race. Each call gets a result that is valid for one snapshot. |

## 5. Conformance and integration tests

The conformance suite in `store/storetest` checks each rule of the [store contract](stores/README.md).
Each adapter of the repository runs it against a real instance of its store.

The integration tests run with each adapter:

- The state stays in the store and a new instance reads it.
- Many instances share the state.
- The store stops and starts again, with and without data loss.
- Reconciliation for each row of the table in [degraded mode](circuit-breaker/degraded-mode.md).
- Cleanup: a state in use stays, and an unused state is removed.
- For the Redis adapter, Redis Cluster: all keys of one breaker are in one slot.

## 6. Acceptance tests

The [requirements](circuit-breaker/requirements.md) define the acceptance tests AT-1 to AT-7.
Each acceptance test is one named test.
It runs against each adapter of the repository.
The roadmap states the milestone in which each test must pass.

## 7. Chaos tests

| Fault | Method | Assertions |
|---|---|---|
| Store unavailable | Stop the container | Requests continue. Local mode starts. No call waits longer than the store timeout. |
| Store slow | Add latency in the proxy | The same as store unavailable. The latency of `CLOSED` calls does not change. |
| Store restart | Restart the container without persistence | `Restore` gives the highest authoritative generation. |
| Store partition | Cut the connection of a part of the instances | The two groups work. After the partition, all instances converge. |
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

The benchmark runs 1, 10, 50, and 100 instances in one process against one store.
It runs with the store healthy, unavailable, and recovering.
It runs for each adapter that shares state between instances.
It records:

- The latency that the breaker adds to a call, at percentiles 50, 99, and 99.9.
- The number of store operations each second, for different request rates.
- The memory for each breaker and the number of goroutines.

The run passes when the store operation rate does not change with the request rate, and when memory and goroutines stay constant during the run.

## 9. Continuous integration

Each change runs: build, `go vet`, static analysis, unit tests with `-race`, conformance tests of each adapter, and integration tests.
Chaos tests and system benchmarks run on a schedule and before a release.
