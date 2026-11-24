# Testing

Faultvane protects production services.
A defect in it can cause an outage or hide one.
For this reason, tests are a primary product of this project, not an addition to it.

A user of the library must be able to trust two statements:

1. Each line of the library ran in a test.
2. Each scenario that the documentation describes has a test that proves it.

## 1. Quality gates

This table gives each gate and its real status.

| Gate | Rule | Runs | Status |
|---|---|---|---|
| Format | `gofmt` reports no file. | Each change | Active |
| Static analysis | `go vet` and `golangci-lint` report nothing. | Each change | Active |
| Unit tests | All tests of the core module pass with the race detector. | Each change, with Go 1.24 and with the current stable Go | Active |
| **Coverage** | **Statement coverage is 98% or more for each module, and 95% or more for each package.** | Each change | Active for the core module, the three adapter modules, and the `otel` module |
| Requirement traceability | Each implemented requirement has a test that names it. | Each change | Active |
| Goroutine leaks | The tests of each policy compare the number of goroutines before and after. | Each change | Active, as a part of the unit tests |
| Conformance | Each store adapter passes the conformance suite against a real store. | Each change | Active |
| Integration | The tests of each adapter with many instances pass against a real store. | Each change | Active |
| Examples | The three example programs build, and the tests of their output pass. | Each change | Active |
| Fuzzing | Each fuzz target runs for 3 minutes and finds nothing. | Each day | Active, as a scheduled workflow |
| System benchmark | Acceptance test AT-7 passes with the Redis adapter. | Each week | Active, as a scheduled workflow |
| Chaos | The fault tests pass. | Each change | Active only as tests of the Redis adapter with a TCP proxy. No separate scheduled gate exists. See section 8. |
| Mutation | The mutation score of each policy package is 90% or more. | Each day | Active, as a scheduled workflow. See section 2. |
| Benchmark comparison | No benchmark is worse than its baseline by more than the permitted margin. | Not automated | **Not active.** No workflow compares a benchmark with its baseline. A person compares the results with [benchmarks](benchmarks.md). |

No active gate has a manual override.
A change that cannot pass a gate changes the code or the test, not the gate.

### Workflows

| Workflow | File | Jobs |
|---|---|---|
| CI | `.github/workflows/ci.yml` | `test`: format, build, `go vet`, coverage, and traceability for the core module. `lint`: `golangci-lint`. `stores`: the adapter modules against real stores, the `otel` module, and the examples. |
| Fuzz | `.github/workflows/fuzz.yml` | Eight fuzz targets, each day, 3 minutes for each target. |
| System benchmark | `.github/workflows/system.yml` | Acceptance test AT-7 with Redis, each week. |
| Scenarios | `.github/workflows/scenarios.yml` | The Docker scenarios with 10 pods, each day. |
| Mutation | `.github/workflows/mutation.yml` | The mutation gate for the seven policy packages, each day. |
| Long run | `.github/workflows/long-run.yml` | One hour of load with a check of the heap and the goroutines, each week. |
| Benchmarks | `.github/workflows/benchmarks.yml` | The core benchmarks on a Linux runner, started by a person. |

For each release, a person runs all workflows on the release commit. No automation enforces this order.

## 2. Coverage rule

### The numbers

| Scope | Minimum |
|---|---|
| Each module, all statements | 98% |
| Each package | 95% |

The script `scripts/coverage.sh` measures and enforces both numbers.
Continuous integration runs it for the core module, for each adapter module, and for the `otel` module.

### How coverage is measured

- The measure is statement coverage from `go test` with `-covermode=atomic` and the race detector.
- All tests of a module count for all packages of the module (`-coverpkg=./...`).
- For a store adapter, the profile includes the tests that run against a real store. An adapter cannot reach the minimum with mocks.

### What does not count against coverage

- The `examples` module. Continuous integration builds its programs and runs the tests of their output, but it does not measure their coverage.

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

The mutation gate is active. The command is `sh scripts/mutation.sh`. The workflow `mutation.yml` runs it each day.

- The tool is `tools/mutation`, a separate module of the repository. It uses only the standard library.
- The tool changes one operator in one file for each mutant: a comparison (`<`, `<=`, `>`, `>=`, `==`, `!=`), a logical operator (`&&`, `||`), or `+` and `-`. It does not change the text of a string.
- For each mutant, the tool runs the tests of the package without the race detector. The mutant is killed when a test fails or when the tests do not complete in the time limit.
- The tool first runs the tests of the package without a mutant. These tests must pass. The time limit for a mutant is 3 times the time of this run plus 5 seconds. The test program ends itself at the limit, so a mutant with a loop that does not end leaves no process.
- The score of a package is the number of killed mutants divided by the number of mutants that compile.
- The gate fails when the score of a package is below 90%. The packages are `circuitbreaker`, `timeout`, `retry`, `bulkhead`, `concurrencylimit`, `loadshed`, and `ratelimit`.
- The tool prints each mutant that survives.

Limits of the gate:

- The gate does not apply to the conformance suites `circuitbreaker/storetest` and `ratelimit/storetest`, to the store adapters, or to the `otel` module. The score of `circuitbreaker/storetest` is 63.2%.
- The tool does not remove statements and does not change constants.
- Some mutants that survive have no effect on the behavior. The tool cannot find them. They decrease the score.

Result on 2026-11-24:

| Package | Mutants | Killed | Score |
|---|---|---|---|
| `circuitbreaker` | 190 | 190 | 100% |
| `timeout` | 6 | 6 | 100% |
| `retry` | 26 | 26 | 100% |
| `bulkhead` | 11 | 11 | 100% |
| `concurrencylimit` | 34 | 34 | 100% |
| `loadshed` | 15 | 15 | 100% |
| `ratelimit` | 60 | 59 | 98.3% |

The mutant that survives in `ratelimit` has no effect on the behavior: a full bucket gives the same result with `>=` and with `>`.

Requirement QA-9 is satisfied for the seven policy packages.

### Scripts in a store

Go coverage cannot measure code that runs in a store, for example a Lua script in Redis.
The conformance suite and the tests of the Redis adapter run each script against a real Redis.
The suite also compares the scripts with the in-memory store for random sequences of operations.
The repository has no written list of the branches of each script. A review of a script change must check that a test runs each branch.

## 3. Test levels

| Level | Subject | Tools | Needs a real store |
|---|---|---|---|
| Unit | Each package: logic, configuration validation, error values | `testing`, fake clock | No |
| Property | Invariants with random event sequences | `math/rand/v2` with fixed seeds | No |
| Fuzz | The state machine, names, and configuration values | `go test -fuzz` | No |
| Concurrency | Request path with many goroutines, races between results and state changes | `-race`, a count of the goroutines | No |
| Conformance | Each store adapter against the store contract | `circuitbreaker/storetest`, real store in a container | Yes, except in-memory |
| Integration | Many instances in one process against one store | In-memory adapter, and real stores from `docker-compose.test.yml` | Both |
| Acceptance | The acceptance tests of the requirements documents | In-memory adapter, and real stores | Both |
| Chaos | Faults of the store and of the dependency | A fake store in the core module. A TCP proxy in the tests of the Redis adapter. | Both |
| Benchmark | Cost of the request path and load on the store | `testing.B`, and the system test of the Redis adapter | Both |
| Example | The example programs give the expected output | Normal tests in the `examples` module | The `shared` example needs Redis |

The repository has no separate `tests` module.
The tests of the core module use only the standard library, so the core module keeps a `go.mod` with no dependencies.
The tests that need a client library are in the module of the adapter or in the `otel` module.

### Examples

The project forbids comments in code.
A Go example test needs an `// Output:` comment to run. For this reason, the project does not use Go example tests that run.

- The packages `bulkhead` and `concurrencylimit` have example functions without an output comment. The compiler checks them, but `go test` does not run them.
- The programs `examples/pipeline` and `examples/shared` each have a normal test. The test runs the program and compares its output with the expected text.

Requirement QA-13 is satisfied only by these two programs. The code blocks in the documents are not compiled by a test.

## 4. Rules for tests

- Write the test first, where it fits. Then write the code. Then refactor.
- Unit tests use a fake clock. They do not sleep.
- Integration tests use short durations and wait for a condition with a deadline. They do not assert on a fixed sleep time. One test of the Redis adapter asserts that 1000 calls with a slow store complete in less than 100 ms.
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
| Observability | Each metric, each log event, and each trace attribute. The circuit breaker has all of these signals. The timeout, retry, bulkhead, concurrency limiter, and load shedding policies have metrics only, and each metric has a test. |
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

The conformance suite in `circuitbreaker/storetest` checks the rules of the [store contract](stores/README.md).
Each adapter of the repository runs it against a real instance of its store: Redis, Redis with cluster mode on, a Redis Cluster of 3 nodes, PostgreSQL, MySQL, and MongoDB.

The conformance suite in `ratelimit/storetest` checks the rules of the [rate limit store](ratelimit/store.md). The in-memory store, the Redis store, the SQL store, and the MongoDB store run it. Each adapter runs it against a real store: Redis, a Redis Cluster of 3 nodes, PostgreSQL, MySQL, and MongoDB.

The tests of local mode, reconciliation, and data loss are in the core module. They use the in-memory adapter and a fake store for faults.

The tests of each adapter with a real store contain:

- Many instances share the state (AT-1) and the probe (AT-2).
- An instance restores the state after the store lost it.
- The data layout and the cleanup of the adapter.
- Each operation returns `faultvane.ErrStoreUnavailable` when the store does not answer.

The tests of each adapter also contain: the connection stops and starts again, with reconciliation (AT-3, AT-4, AT-6).

Only the tests of the Redis adapter also contain:

- A slow store delays no call.
- One Redis server with cluster mode on: all keys of one breaker are in one slot. The key of the rate limit store for the same name is in that slot also, and the rate limit conformance suite passes with a cluster client.

[Stores](stores/README.md) has a table of the acceptance tests for each adapter.

## 8. Circuit breaker: chaos tests

The chaos tests are normal tests. They run with each change.
No separate scheduled chaos gate exists, and no test stops or restarts a container.

| Fault | Method | Where | Assertions |
|---|---|---|---|
| Store unavailable | A fake store returns errors. A TCP proxy stops the connection to Redis. A test closes the connections to PostgreSQL, MySQL, and MongoDB and refuses new connections. | Core module, each adapter | Requests continue. Local mode starts. After the connection is back, the instance reconciles. |
| Store slow | The TCP proxy adds a delay of 100 ms. | Redis adapter | Local mode starts. 1000 calls complete in less than 100 ms. |
| Store data loss | The test deletes the data of the breaker in the store. | Core module, each adapter | `Restore` writes the last authoritative snapshot again. The highest generation wins. |
| Store partition | The test stops the connection of a part of the instances. | Each adapter | The instances get different local states. After the partition, all instances converge to the state of the store. |
| Dependency unavailable | The fake dependency fails. | Core module, each adapter | The circuit opens for all instances. |
| Dependency recovery | The fake dependency succeeds again. | Core module, each adapter | One probe. The circuit closes for all instances. |
| Application restart | The test creates a new instance during `OPEN`. | Core module | The new instance reads `OPEN`. |

A Redis Cluster of 3 primary nodes is tested, with breakers on each node, a notification between two nodes, and one slot move.
Not tested: a restart of a real store container, a failover of Redis Sentinel, of a Redis Cluster, or of a database.

## 9. Scenarios in Docker

The scenarios run 10 pod containers, a controllable dependency, and four real stores.
Two scenarios check the shared rate limit with the Redis store: the bound under heavy load, and local mode during a Redis outage.
They test each policy with separate processes and a real network, and they stop and start store containers.
[Scenarios in Docker](scenarios.md) lists each scenario and its expected result.

```sh
sh scripts/scenarios.sh
```

The workflow `scenarios.yml` runs them each day.

## 9a. Requirement traceability

Each requirement and each acceptance test has an identifier, for example `GEN-3` or `AT-5`.

- The name of an acceptance test contains its identifier, for example `TestAT5_StaleWriterIsRejected`.
- A test that proves a requirement names the identifier in a subtest name or in the test table.
- The script `scripts/traceability.sh` runs in continuous integration. It reads `scripts/implemented-requirements.txt`, the list of the requirements that the completed milestones implement. The list has 301 identifiers. It fails when an identifier of the list has no test, or when a test names an identifier that the requirements documents do not define.
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

The system benchmark is acceptance test AT-7. It is the test `TestSystem` in the module `store/redis`.
It runs 1, 10, 50, and 100 instances in one process against one Redis.
It runs with the store healthy, unavailable, and recovering, and with a low and a high request rate.
It records:

- The latency that the breaker adds to a call, at percentiles 50, 99, and 99.9.
- The number of store operations each second.
- The number of synchronizer goroutines during the run and after `Close`.

The run fails in each of these conditions:

- Percentile 99 of the added latency is above 1 ms.
- The store operation rate is above 3 operations each second for each instance.
- The high request rate makes less than 10 times the calls of the low request rate.
- The number of synchronizer goroutines is not equal to the number of instances during the run, or is not 0 after `Close`.
[Benchmarks](benchmarks.md) records the results and gives the command.

The system benchmark runs only with the Redis adapter. It does not measure memory, and it runs for 1.5 seconds for each row. The project has no long run for memory.

## 11. Run the tests

The core module needs only Go:

```sh
go build ./...
go vet ./...
sh scripts/coverage.sh
sh scripts/traceability.sh
```

The adapter modules need Docker:

```sh
sh scripts/test-stores.sh
```

`scripts/test-stores.sh` starts Redis, a Redis with cluster mode on, a Redis Cluster of 3 nodes, PostgreSQL, MySQL, and MongoDB from `docker-compose.test.yml`.
It runs `scripts/coverage.sh` in each adapter module and then removes the containers.
The variable `KEEP_STORES=1` keeps the containers. The variable `STORE_MODULES` selects the modules, for example `STORE_MODULES=store/sql`.

The `otel` module and the `examples` module have their own commands:

```sh
(cd otel && sh ../scripts/coverage.sh)
(cd examples && go vet ./... && go test -count=1 ./...)
```

The test of `examples/shared` needs the Redis of `docker-compose.test.yml`.

`scripts/coverage.sh` runs all tests of the module with the race detector, prints the coverage of each package, and fails below the minimum.
