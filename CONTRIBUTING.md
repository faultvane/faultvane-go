# Contributing

Thank you for your interest in Faultvane.

## Before you start

- Read the [requirements](docs/requirements.md) and the [architecture](docs/architecture.md).
- For a change of behavior, open an issue first. Describe the problem, not only the solution.
- For a change of an important design decision, add a decision record in `docs/adr/`.

## Development

You need Go 1.24 or later and Docker. The integration tests start each store, for example Redis, in a container.

```sh
go build ./...
go vet ./...
go test -race ./...
```

The roadmap adds the commands for integration tests, chaos tests, and benchmarks when these tests exist.

## Rules for code

- Write idiomatic Go. Run `gofmt` and `go vet`.
- Do not write comments in code. Use clear names and small functions.
- The core module has no dependencies outside the standard library.
- Each function that can block accepts a `context.Context`.
- A store adapter contains no behavior that is not storage. It must pass the conformance suite.
- Each goroutine has an owner that stops it. Tests fail on a goroutine leak.
- The request path does not allocate memory and does not call the store in the `CLOSED` state.

## Rules for tests

- Use test-driven development where it fits.
- Test observable behavior, not implementation details.
- Tests are deterministic. Use the fake clock. Do not use `time.Sleep` to wait for a result.
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
- The metric names and label names.

A change must let old and new library versions run against the same store at the same time.
