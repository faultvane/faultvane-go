# Contributing

Thank you for your interest in Faultvane.

## Before you start

- Read the [requirements](docs/requirements.md) and the [architecture](docs/architecture.md). For a change of one policy, read the documents of that policy also.
- For a change of behavior, open an issue first. Describe the problem, not only the solution.
- For a change of an important design decision, add a decision record in `docs/adr/`.

## Development

You need Go and Docker.
The core module needs Go 1.24 or later. The modules `store/sql` and `store/mongodb` need Go 1.25 or later. The modules `store/redis`, `otel`, and `examples` need Go 1.26 or later.

### Modules

The repository contains six Go modules. Each has its own `go.mod` file.

| Module | Directory | Content |
|---|---|---|
| Core | `.` | The root package and all policies. Standard library only. |
| Redis adapter | `store/redis` | Package `redisstore` |
| SQL adapter | `store/sql` | Package `sqlstore` |
| MongoDB adapter | `store/mongodb` | Package `mongostore` |
| OpenTelemetry | `otel` | Package `faultvaneotel` |
| Examples | `examples` | The programs `pipeline`, `shared`, and `grpc` |
| Tools | `tools` | The program `mutation` for the mutation gate |

Each separate module has a `replace` directive that points to the core in the repository.
A `go` command in the repository root applies only to the core module. Run the command in the directory of a different module to build or test that module.

### Core module

```sh
go build ./...
go vet ./...
sh scripts/coverage.sh
sh scripts/traceability.sh
```

`scripts/coverage.sh` runs all tests with the race detector and checks the coverage minimum.
`scripts/traceability.sh` checks that each implemented requirement has a test. When you implement a requirement, add its identifier to `scripts/implemented-requirements.txt` and name the identifier in the test.

### Store tests

```sh
sh scripts/test-stores.sh
```

This script needs Docker.
It starts Redis, a Redis with cluster mode on, PostgreSQL, MySQL, and MongoDB from `docker-compose.test.yml`.
It runs `scripts/coverage.sh` in each adapter module against these stores.
Then it removes the containers.

| Variable | Effect |
|---|---|
| `KEEP_STORES=1` | The script does not remove the containers. |
| `STORE_MODULES` | The modules to test, for example `STORE_MODULES="store/sql"`. The default is all three adapter modules. |

### OpenTelemetry module and examples

```sh
(cd otel && sh ../scripts/coverage.sh)
(cd examples && go vet ./... && go test -count=1 ./...)
```

The test of `examples/shared` needs the Redis of `docker-compose.test.yml`. Start the stores with `KEEP_STORES=1 sh scripts/test-stores.sh` or with `docker compose -f docker-compose.test.yml up -d --wait`.

### System benchmark

```sh
(cd store/redis && FAULTVANE_SYSTEM_TEST=1 go test -count=1 -run TestSystem -v ./...)
```

This command runs acceptance test AT-7 against the Redis of `docker-compose.test.yml`. [Benchmarks](docs/benchmarks.md) records the results.

## Scenario tests

```sh
sh scripts/scenarios.sh
```

This command needs Docker. It starts 10 pods, a dependency, and four stores, and runs each scenario of `docs/scenarios.md`.
Run it when you change the behavior of a policy or of a store adapter.

## Mutation tests

```sh
sh scripts/mutation.sh
```

This command changes one operator of the code at a time and runs the tests of the package. It fails when the mutation score of a policy package is below 90%.
A full run takes about 25 minutes. To examine one package, give its directory: `sh scripts/mutation.sh retry`.

## Rules for code

- Write idiomatic Go. Run `gofmt` and `go vet`.
- Do not write comments in code. Use clear names and small functions.
- The core module has no dependencies outside the standard library.
- Each function that can block accepts a `context.Context`.
- A store adapter contains no behavior that is not storage. It must pass the conformance suite in `circuitbreaker/storetest`. A store of the rate limiter must pass the conformance suite in `ratelimit/storetest`.
- An adapter written in Go uses the type `circuitbreaker.Record` for the transitions. It does not copy the transition logic.
- Each goroutine has an owner that stops it. Tests fail on a goroutine leak.
- The request path does not allocate memory and does not call the store in the `CLOSED` state.

## Rules for tests

Tests are the most important part of a change. [Testing](docs/testing.md) gives all rules.

- The statement coverage must stay at 98% or more for each module and 95% or more for each package. Continuous integration rejects a change below these values.
- Do not exclude code from the coverage measurement.
- A change of behavior without a test for that behavior is not accepted, also when the coverage number is sufficient.
- Use test-driven development where it fits.
- Test observable behavior, not implementation details.
- Tests are deterministic. Use the clock of the package `faultvanetest`. Do not use `time.Sleep` to wait for a result. A test against a real store waits for a condition with a time limit.
- When you fix a defect, add a regression test.
- Do not weaken or skip a valid test.

## Rules for commits

- Each commit has one meaning.
- Write the subject in the imperative mood, for example `Add probe lease expiry`.
- Do not mix a refactor with a change of behavior.

## Rules for documentation

- Write in Simplified Technical English: short sentences, active voice, one idea in each sentence.
- Use the same word for the same thing. The [glossary](docs/README.md#glossary) defines the terms.
- When behavior changes, update the related document in the same change.

## License of contributions

The project has the Apache License 2.0.
When you send a change, you agree that your contribution has the same license.

## Compatibility

These items are commitments after version 1.0:

- The public Go API.
- The `Store` interface and its contract.
- The data layout of each store adapter.
- The metric names and attribute names.

A change must let old and new library versions run against the same store at the same time.
