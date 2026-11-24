# Testing

Faultvane protects production services.
A defect in it can cause an outage or hide one.
For this reason, tests are a primary product of this project, not an addition to it.

A user of the library must be able to trust two statements:

1. Each line of the library ran in a test.
2. Each scenario that the documentation describes has a test that proves it.

## 1. Quality gates

Continuous integration rejects a change that does not pass all gates.

| Gate | Rule | Runs |
|---|---|---|
| Format | `gofmt` reports no file. | Each change |
| Static analysis | `go vet` and the linter report nothing. | Each change |
| Unit tests | All tests pass with the race detector. | Each change |
| **Coverage** | **Statement coverage is 98% or more for each module, and 95% or more for each package.** | Each change |
| Conformance | Each store adapter passes the conformance suite against a real store. | Each change |
| Integration | The integration tests and the acceptance tests pass with each adapter. | Each change |
| Goroutine leaks | No test leaves a goroutine. | Each change |
| Chaos | The fault tests pass. | Each day and before a release |
| Fuzzing | The fuzz targets run for a fixed time and find nothing. | Each day and before a release |
| Mutation | The mutation score of the critical packages is 90% or more. | Each week and before a release |
| Benchmarks | No benchmark is worse than its baseline by more than the permitted margin. | Before a release |

No gate has a manual override.
A change that cannot pass a gate changes the code or the test, not the gate.

Active now: format, static analysis, unit tests, coverage, goroutine leaks, requirement traceability, and fuzzing.
Not active yet: the mutation gate. Section 2 gives the reason.
Each other gate becomes active in the milestone that adds its tests. The [roadmap](roadmap.md) names the milestone.

## 2. Coverage rule

### The numbers

| Scope | Minimum |
|---|---|
| Each module, all statements | 98% |
| Each package | 95% |

The script `scripts/coverage.sh` measures and enforces both numbers.
Continuous integration runs it for each module.

### How coverage is measured

- The measure is statement coverage from `go test` with `-covermode=atomic` and the race detector.
- All tests of a module count for all packages of the module (`-coverpkg=./...`).
- For a store adapter, the profile includes the tests that run against a real store. An adapter cannot reach the minimum with mocks.

### What does not count against coverage

- The `examples` module. Its programs are built and run in continuous integration, but they are not measured.

No other exclusion exists.
The project has no list of ignored files and no "ignore coverage" markers.

### Code that a test cannot reach

- Code that no test can reach is dead code. Remove it.
- A branch for an error that "cannot occur" is a decision. Make the error possible to inject, and test it, or remove the branch.
- The 2% margin is for statements that are too expensive to trigger in a deterministic test. Each such statement is visible in the coverage report. A reviewer must accept it.

### Coverage is necessary, not sufficient

A test can run a line and assert nothing.
Coverage does not find this problem.
Three other gates find it:

- **Mutation tests** change the code, for example `>=` to `>`. A test must fail for each change. The state machine, the window, the failure condition, and the synchronizer must have a mutation score of 90% or more.
- **Requirement traceability.** Each requirement identifier and each acceptance test of the requirements documents has a named test. Section 9 gives the rule.
- **Property tests** check the invariants with random inputs that no person wrote.

### Status of the mutation gate

The mutation gate needs an external tool in continuous integration. The tool is not selected and not tested yet.
Until then, the maintainers run mutations by hand before the end of a milestone and record the result in the roadmap.

### Scripts in a store

Go coverage cannot measure code that runs in a store, for example a Lua script in Redis.
For such code, the adapter keeps a list of the branches of each script.
The conformance suite must contain a test for each branch.
A review of a script change checks the list.

## 3. Test levels

| Level | Subject | Tools | Needs a real store |
|---|---|---|---|
| Unit | Each package: logic, configuration validation, error values | `testing`, fake clock | No |
| Property | Invariants with random event sequences | Property-based testing | No |
| Fuzz | Inputs from outside: store responses, configuration values | `go test -fuzz` | No |
| Concurrency | Request path with many goroutines, races between results and state changes | `-race`, goroutine leak detector | No |
| Conformance | Each store adapter against the store contract | `store/storetest`, real store in a container | Yes, except in-memory |
| Integration | Many instances in one process against one store | In-memory adapter, Testcontainers | Both |
| Acceptance | The acceptance tests of the requirements documents | Real store in a container | Yes |
| Chaos | Faults of the store and of the dependency | Network fault proxy, container control | Yes |
| Benchmark | Cost of the request path and load on the store | `testing.B` | Both |
| Example | The examples in the documentation compile and give the documented output | Go example tests | No |

Tests that need libraries outside the standard library are in a separate `tests` module.
The core module keeps a `go.mod` with no dependencies.

## 4. Rules for tests

- Write the test first, where it fits. Then write the code. Then refactor.
- Unit tests use a fake clock. They do not sleep.
- Integration tests use short durations and wait for a condition with a deadline. They do not assert on a fixed sleep time.
- A test that fails sometimes is a defect. Continuous integration does not run a failed test again.
- Each test creates its own namespace. Tests can run in parallel on one store.
- Tests assert on observable behavior: the result of a call, the state, the number of calls that reach the dependency, and the state that the store returns.
- A fake dependency counts the calls that it receives. Tests of protection assert on this count.
- Tests of the core use the in-memory adapter. It is a real implementation of the contract, not a mock.
- A fake store that returns errors or waits is permitted for tests of store faults.
- Each fix of a defect adds a regression test that fails without the fix.
- Nobody weakens, deletes, or skips a valid test to make a change pass.

## 5. Tests for each policy

Each policy must have tests for these subjects before its release:

| Subject | Content |
|---|---|
| Decision | Each path of the decision: permit and each type of rejection. |
| Configuration | Each validation rule, with a valid value and a value that is not valid. Each default. |
| Errors | Each error value matches as the documentation says. The error of the call is not changed. |
| Context | Cancellation before, during, and after the call. Deadline. |
| Panic | A panic of the call is recorded and raised again. |
| Concurrency | Many goroutines. No data race. Limits hold exactly under contention. |
| Lifecycle | `Close` stops all goroutines. Calls after `Close` have a defined result. |
| Composition | The policy in a pipeline with each other policy that exists. |
| Observability | Each metric, each log event, and each trace attribute. |
| Performance | A benchmark of the normal path and an assertion of zero allocations. |

## 6. Circuit breaker: unit tests

| Area | Cases |
|---|---|
| `CLOSED` | Permits calls. Records results. Opens at the count threshold, at the rate threshold, and at the slow call threshold. Does not open below `MinimumCalls`. |
| `OPEN` | Rejects calls. Does not call the dependency. Stays `OPEN` before the open duration ends. |
| `HALF_OPEN` | Permits the probe limit. Rejects the other calls. Closes at the success threshold. Opens at one failure. |
| Window | Buckets rotate. Old buckets do not count. Totals are correct at bucket edges. |
| Classifier | Default rule. Custom rule. Cancellation by the caller is ignored. |
| Local mode | Entry from each state. Local thresholds. Time limit and each expired policy. Each start policy. |
| State machine | Each transition T1 to T4. Each invalid transition. Each generation rule G1 to G5. |

Property tests check the six invariants of the [state machine](circuit-breaker/state-machine.md).

## 7. Circuit breaker: concurrency, conformance, and integration tests

| Case | Expected result |
|---|---|
| Many instances send a report that reaches the threshold at the same time. | One transition T1. The generation increases by 1. |
| Many instances request a probe at the same time. | The number of granted leases is equal to the probe limit. |
| An instance writes with an old generation. | The store ignores the write. |
| A probe result arrives after its lease ended. | The store ignores the result. |
| Many instances reconcile at the same time with different local states. | All instances end with the same state and generation. |
| Many goroutines call `Execute` while the snapshot changes. | No data race. Each call gets a result that is valid for one snapshot. |

The conformance suite in `store/storetest` checks each rule of the [store contract](stores/README.md).
Each adapter of the repository runs it against a real instance of its store.

The integration tests run with each adapter:

- The state stays in the store and a new instance reads it.
- Many instances share the state.
- The store stops and starts again, with and without data loss.
- Reconciliation for each row of the table in [degraded mode](circuit-breaker/degraded-mode.md).
- Cleanup: a state in use stays, and an unused state is removed.
- For the Redis adapter, Redis Cluster: all keys of one breaker are in one slot.

## 8. Circuit breaker: chaos tests

| Fault | Method | Assertions |
|---|---|---|
| Store unavailable | Stop the container | Requests continue. Local mode starts. No call waits longer than the store timeout. |
| Store slow | Add latency in the proxy | The same as store unavailable. The latency of `CLOSED` calls does not change. |
| Store restart | Restart the container without persistence | `Restore` gives the highest authoritative generation. |
| Store partition | Cut the connection of a part of the instances | The two groups work. After the partition, all instances converge. |
| Dependency unavailable | The fake dependency fails | The circuit opens for all instances. The traffic stops. |
| Dependency recovery | The fake dependency succeeds again | One probe. The circuit closes for all instances. |
| Application restart | Create a new instance during `OPEN` | The new instance reads `OPEN`. It sends no call. |

## 9. Requirement traceability

Each requirement and each acceptance test has an identifier, for example `GEN-3` or `AT-5`.

- The name of an acceptance test contains its identifier, for example `TestAT5_StaleWriterIsRejected`.
- A test that proves a requirement names the identifier in a subtest name or in the test table.
- The script `scripts/traceability.sh` runs in continuous integration. It reads `scripts/implemented-requirements.txt`, the list of the requirements that the completed milestones implement. It fails when an identifier of the list has no test, or when a test names an identifier that the requirements documents do not define.
- A requirement about the documentation or about the process, for example TK-4 or QA-3, has no test in the code. A review checks it.

## 10. Benchmarks

### Request path

| Benchmark | Target |
|---|---|
| `Execute` on a circuit breaker in `CLOSED`, one goroutine | Less than 100 ns, 0 allocations |
| `Execute` in `CLOSED`, parallel on all cores | No drop of throughput from contention |
| `Execute` in `OPEN` | Less than 50 ns, 0 allocations |

These values are targets.
[Benchmarks](benchmarks.md) records the measured results. The first measurement of each benchmark is its baseline.
A later change must not make the baseline worse without a documented reason.
A test asserts zero allocations with `testing.AllocsPerRun`, so this property is part of the normal test run.

### System

The benchmark runs 1, 10, 50, and 100 instances in one process against one store.
It runs with the store healthy, unavailable, and recovering.
It runs for each adapter that shares state between instances.
It records:

- The latency that the policy adds to a call, at percentiles 50, 99, and 99.9.
- The number of store operations each second, for different request rates.
- The memory for each policy instance and the number of goroutines.

The run passes when the store operation rate does not change with the request rate, and when memory and goroutines stay constant during the run.

## 11. Run the tests

```sh
go build ./...
go vet ./...
sh scripts/coverage.sh
sh scripts/traceability.sh
```

`scripts/coverage.sh` runs all tests of the module with the race detector, prints the coverage of each package, and fails below the minimum.
