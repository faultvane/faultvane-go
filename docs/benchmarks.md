# Benchmarks

This document records measured results.
Each result names the commit range, the machine, and the Go version.
A statement about performance in a different document must refer to a result here.

## Conditions of all results

| Item | Value |
|---|---|
| Date | 2026-11-24 |
| Machine | Apple M4, 10 cores, macOS |
| Go | 1.27.3 |
| Clock | System clock |
| Stores | Redis, PostgreSQL, MySQL, and MongoDB in local Docker containers, from `docker-compose.test.yml` |

All results come from this one machine, except the section "Core benchmarks on Linux".
No result comes from a Linux server that is typical for production. The absolute values can be different there.

## Run the benchmarks

```sh
go test -run '^$' -bench . -benchmem -count=3 ./...
```

This command runs the benchmarks of all policies in the core module.
The sections for the store operations and for the system benchmark give their own commands.

## Circuit breaker: request path

### Baseline of milestone M1: local circuit breaker

| Benchmark | Time for each call | Allocations | Target |
|---|---|---|---|
| `Do`, closed, one goroutine | 83 ns | 0 | Less than 100 ns, 0 allocations |
| `Execute`, closed, one goroutine | 83 ns | 0 | Less than 100 ns, 0 allocations |
| `Allow` and `Success`, closed, one goroutine | 82 ns | 0 | Less than 100 ns, 0 allocations |
| `Do`, closed, 10 goroutines in parallel | 64 ns | 0 | No drop of throughput |
| `Do`, open | 31 ns | 0 | Less than 50 ns, 0 allocations |

Each value is the middle result of three runs.

### After shared state was added

The same benchmarks, after the code for the store, local mode, and the counters of `Stats` was added.
The breaker of these benchmarks has no store.

| Benchmark | Time for each call | Allocations | Target |
|---|---|---|---|
| `Do`, closed, one goroutine | 84 ns | 0 | Less than 100 ns, 0 allocations |
| `Execute`, closed, one goroutine | 85 ns | 0 | Less than 100 ns, 0 allocations |
| `Allow` and `Success`, closed, one goroutine | 84 ns | 0 | Less than 100 ns, 0 allocations |
| `Do`, closed, 10 goroutines in parallel | 66 ns | 0 | No drop of throughput |
| `Do`, open | 36 ns | 0 | Less than 50 ns, 0 allocations |

The closed path is 1 ns to 2 ns slower than the baseline. The open path is 5 ns slower.
All targets are still satisfied on this machine.

### What the results show

- The targets of requirement PF-2 are satisfied on this machine.
- A test asserts zero allocations in each test run, so this property does not depend on a benchmark.
- With 10 goroutines, the total throughput is approximately 15 million calls each second. With one goroutine it is approximately 12 million. The throughput does not drop, but it also does not increase much with more cores.

### Known limit

All goroutines add to the same counters of the current window bucket.
The processor cores then compete for the same memory.
This is the reason for the small increase of throughput with more cores.

No change is planned now.
15 million calls each second through one breaker is far above the rate of a real service.
Counters that are divided between cores can remove this limit. That change needs a measured requirement first.

## Other policies

The values are nanoseconds for each call. Each benchmark is in the file `bench_test.go` of its package.

| Policy | Benchmark | Time for each call | Allocations |
|---|---|---|---|
| Timeout | `Do` | 171 ns | 4 |
| Timeout | `Do`, parallel | 95 ns | 4 |
| Retry | `Do`, success at the first attempt | 2.4 ns | 0 |
| Retry | `Do`, success at the first attempt, with a budget | 3.1 ns | 0 |
| Retry | `Do`, three failed attempts | 33 ns | 0 |
| Bulkhead | `Do` | 9.5 ns | 0 |
| Bulkhead | `Do`, parallel | 183 ns | 0 |
| Bulkhead | `Do`, rejection | 4.5 ns | 0 |
| Concurrency limiter | `Do` | 8.0 ns | 0 |
| Concurrency limiter | `Do`, with a latency threshold | 68 ns | 0 |
| Concurrency limiter | `Do`, parallel | 225 ns | 0 |
| Concurrency limiter | `Do`, rejection | 2.2 ns | 0 |
| Rate limiter | Permitted call | 41 ns | 0 |
| Rate limiter | Rejected call | 41 ns | 0 |
| Rate limiter | Parallel | 93 ns to 136 ns | 0 |
| Load shedding | Admitted call | 5.4 ns | 0 |
| Load shedding | Admitted call, with a priority | 20 ns | 0 |
| Load shedding | Shed call | 3.7 ns | 0 |
| Load shedding | Parallel | 187 ns | 0 |

### What the results show

- Each policy adds less than 1 microsecond to a call on this machine. Requirement TK-51 asks for less than 1 ms.
- The timeout policy makes 4 allocations for each call, because it makes a context with a deadline. All other policies make 0 allocations on these paths.
- The parallel benchmarks of the bulkhead, the concurrency limiter, the rate limiter, and the load shedding policy are slower than the benchmarks with one goroutine. The probable cause is that all goroutines change the same counters, and the processor cores compete for the same memory. The project did not measure the cause.

## Store operations

One client sends the operations in sequence to a store in a local container.
The value is the time of one operation, with the round trip to the container.

| Adapter | Store | `Load` | `Report` |
|---|---|---|---|
| Redis | Redis | Not recorded | 0.13 ms |
| SQL | PostgreSQL | 0.29 ms | 0.86 ms |
| SQL | MySQL | 0.26 ms | 2.0 ms |
| MongoDB | MongoDB | 0.17 ms | 0.50 ms |

How to run, with the stores of `docker-compose.test.yml` in operation:

```sh
docker compose -f docker-compose.test.yml up -d --wait
(cd store/redis && go test -run '^$' -bench BenchmarkOperations ./...)
(cd store/sql && go test -run '^$' -bench BenchmarkOperations ./...)
(cd store/mongodb && go test -run '^$' -bench BenchmarkOperations ./...)
```

### What the results show

- Each operation is far below the default store timeout of 100 ms on this machine.
- The store operations are not on the path of a normal call. Their latency sets the lowest practical sync interval, not the latency of a call.
- A real network adds its round-trip time. The SQL adapter makes more than one round trip for each write operation. The MongoDB adapter makes two.

## System benchmark: acceptance test AT-7

The test `TestSystem` in the module `store/redis` runs many breaker instances in one process against one Redis.

| Item | Value |
|---|---|
| Adapter | Redis, through a TCP proxy in the test |
| Configuration | The defaults, except a store timeout of 80 ms and a start timeout of 2 s |
| Time for each row | 1.5 s |
| Request rate "low" | Approximately 800 calls each second for each instance |
| Request rate "high" | Calls without a pause |
| Store "unavailable" | The proxy stops the connection before the run. |
| Store "recovering" | The proxy stops the connection before the run and starts it again after half of the time. |

| Instances | Store | Request rate | Calls | p50 | p99 | p99.9 | Store operations each second |
|---|---|---|---|---|---|---|---|
| 1 | healthy | low | 1185 | 875ns | 1.6µs | 3.1µs | 1 |
| 1 | healthy | high | 7915682 | 125ns | 167ns | 250ns | 1 |
| 1 | unavailable | high | 7896655 | 125ns | 167ns | 250ns | 1 |
| 1 | recovering | high | 7845125 | 125ns | 167ns | 250ns | 1 |
| 10 | healthy | low | 11830 | 625ns | 1.1µs | 3.4µs | 18 |
| 10 | healthy | high | 29839282 | 166ns | 3.6µs | 7.2µs | 13 |
| 10 | unavailable | high | 26705737 | 167ns | 3.8µs | 7.3µs | 13 |
| 10 | recovering | high | 31683831 | 166ns | 3.6µs | 7.2µs | 13 |
| 50 | healthy | low | 60050 | 583ns | 1.1µs | 3.2µs | 99 |
| 50 | healthy | high | 43610623 | 125ns | 3.1µs | 7.0µs | 67 |
| 50 | unavailable | high | 23650405 | 125ns | 4.0µs | 7.3µs | 72 |
| 50 | recovering | high | 50851046 | 125ns | 416ns | 6.0µs | 67 |
| 100 | healthy | low | 131800 | 542ns | 1.0µs | 3.6µs | 199 |
| 100 | healthy | high | 38853489 | 125ns | 3.3µs | 7.0µs | 134 |
| 100 | unavailable | high | 37820110 | 125ns | 3.5µs | 7.1µs | 149 |
| 100 | recovering | high | 44926680 | 125ns | 2.8µs | 6.8µs | 133 |

The test also runs the low request rate with the store unavailable and recovering. This table does not record those rows.

The times include the two clock reads of the measurement.

### What the results show

- **PF-3.** The store operation rate is approximately 2 operations each second for each instance, or less. It does not increase with the request rate: 100 instances make 199 operations each second at the low rate and 134 at the high rate, while the number of calls is approximately 300 times larger.
- **PF-2.** The latency that the breaker adds stays in microseconds in all three store conditions. The highest percentile 99.9 of the table is 7.3 µs.
- **PF-6.** The number of synchronizer goroutines is equal to the number of instances during the run, and it is 0 after `Close`. The test asserts both.

### How to run

Start Redis from `docker-compose.test.yml`. Then run this command in the directory `store/redis`:

```sh
FAULTVANE_SYSTEM_TEST=1 go test -count=1 -run TestSystem -v ./...
```

The workflow `.github/workflows/system.yml` runs the same command each week.

## Core benchmarks on Linux

| Item | Value |
|---|---|
| Date | 2026-11-24 |
| Commit | `ce6c88c` |
| Machine | GitHub runner `ubuntu-latest`, AMD EPYC 7763, 4 processors |
| Go | The stable version of the runner |
| Command | The workflow `benchmarks.yml`: `go test -run '^$' -bench . -benchmem -count 5 ./...` |

Each value is the median of 5 runs.

| Policy | Benchmark | Time for each call | Allocations |
|---|---|---|---|
| Circuit breaker | `Do`, closed | 153 ns | 0 |
| Circuit breaker | `Do`, closed, parallel | 64 ns | 0 |
| Circuit breaker | `Do`, open | 74 ns | 0 |
| Timeout | `Do` | 395 ns | 4 |
| Timeout | `Do`, parallel | 189 ns | 4 |
| Retry | `Do`, success | 4.4 ns | 0 |
| Retry | `Do`, success with a budget, parallel | 18 ns | 0 |
| Retry | Three failed attempts | 73 ns | 0 |
| Bulkhead | `Do` | 16 ns | 0 |
| Bulkhead | `Do`, parallel | 60 ns | 0 |
| Concurrency limiter | `Do` | 18 ns | 0 |
| Concurrency limiter | `Do`, with a latency threshold | 128 ns | 0 |
| Concurrency limiter | `Do`, parallel | 87 ns | 0 |
| Rate limiter | Permitted call | 70 ns | 0 |
| Rate limiter | Permitted call, parallel | 71 ns | 0 |
| Rate limiter | Permitted call with a store | 70 ns | 0 |
| Rate limiter | Permitted call with a store, parallel | 75 ns | 0 |
| Load shedding | Admitted call | 11 ns | 0 |
| Load shedding | Admitted call, parallel | 105 ns | 0 |

What the results show:

- Each policy adds less than 1 µs to a call on this machine. The targets of requirement PF-2 are satisfied.
- A rate limiter with a store has the same cost for a call as a rate limiter without a store.
- A runner is a shared virtual machine. The values change between runs. They do not replace a measurement on the servers of an application.
- The store operations and the system benchmark have no result from Linux in this document.

## Propagation of a transition

| Item | Value |
|---|---|
| Date | 2026-11-24 |
| Machine | Apple M4, 10 cores, macOS, Redis 7.4 in a local container |
| Options | Sync interval 500 ms, early report interval 50 ms, one reader |
| Method | A temporary test measured 40 transitions for each row, two times. The table gives the less favorable run. The test is not in the repository. |

| Configuration | Median | 90th percentile | Maximum |
|---|---|---|---|
| Redis adapter with `WithNotifications` | 0.9 ms | 1.2 ms | 43 ms |
| Without notifications | 300 ms | 470 ms | 490 ms |

The maximum with notifications occurs when the instance synchronized less than one early report interval before the notification.

## Long run: memory and goroutines

| Item | Value |
|---|---|
| Date | 2026-11-24 |
| Machine | Apple M4, 10 cores, macOS |
| Command | `FAULTVANE_LONG_RUN=1h go test -count=1 -timeout 70m -run TestLongRun -v .` |
| Load | 4 goroutines call one pipeline of all seven policies without a pause. The circuit breaker and the rate limiter use an in-memory store. The dependency fails for 2 seconds in each 10 seconds. |

The test reads the heap after a garbage collection 20 times, one time each 3 minutes.

| Measurement | Result |
|---|---|
| Calls | 2 949 million successes, 22 million failures |
| Transitions | The circuit opened 24 times |
| Heap in use | 324 kB to 350 kB in each sample |
| Goroutines | 8 in each sample during the load |

What the results show:

- The heap does not grow in one hour. Requirement PF-5 is satisfied for this load.
- The number of goroutines does not grow. Requirement PF-6 is satisfied for this load.
- The test fails when a sample after the first quarter of the run is more than 1 MiB above the largest sample of the first quarter, or when it has more goroutines.
- The run does not use a real store, and it does not measure the memory of the process as the operating system counts it.

The workflow `long-run.yml` runs the same test each week on a GitHub runner.

## What is not measured

- A machine that is typical for production, for example a Linux server.
- Stores on a different host, with a real network.
- The system benchmark with the SQL adapter and with the MongoDB adapter.
- `Load` of the Redis adapter, and the operations `AcquireProbe`, `CompleteProbe`, and `Restore` of each adapter.
- The memory of an instance with a real store.
- The lowest practical sync interval for each adapter.
