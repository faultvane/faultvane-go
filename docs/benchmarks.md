# Benchmarks

This document records measured results.
Each result names the commit range, the machine, and the Go version.
A statement about performance in a different document must refer to a result here.

## Run the benchmarks

```sh
go test -run '^$' -bench . -benchmem -count=3 ./circuitbreaker
```

## Baseline of milestone M1: local circuit breaker

| Item | Value |
|---|---|
| Date | 2026-11-24 |
| Machine | Apple M4, 10 cores, macOS |
| Go | 1.27.3 |
| Clock | System clock |

| Benchmark | Time for each call | Allocations | Target |
|---|---|---|---|
| `Do`, closed, one goroutine | 83 ns | 0 | Less than 100 ns, 0 allocations |
| `Execute`, closed, one goroutine | 83 ns | 0 | Less than 100 ns, 0 allocations |
| `Allow` and `Success`, closed, one goroutine | 82 ns | 0 | Less than 100 ns, 0 allocations |
| `Do`, closed, 10 goroutines in parallel | 64 ns | 0 | No drop of throughput |
| `Do`, open | 31 ns | 0 | Less than 50 ns, 0 allocations |

Each value is the middle result of three runs.

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

### What is not measured yet

- A machine that is typical for production, for example a Linux server.
- The breaker with a shared store. That is part of milestone M2 and milestone M6.
