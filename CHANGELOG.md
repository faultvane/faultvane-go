# Changelog

This file records all important changes of the project.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Core package: `ErrRejected`, `ErrInvalidConfig`, `Clock`, `Policy`, `Combine`, and `Execute`.
- Package `circuitbreaker`: a local circuit breaker with a time-based sliding window, failure rate, failure count, and slow call conditions, coordinated half-open probes with leases, generations, and a classifier.
- Package `faultvanetest`: a clock that a test controls.
- Requirement traceability check and a scheduled fuzz workflow.
- Benchmark baseline for the local circuit breaker.

- Repository foundation.
- Apache License 2.0.
- Store interface with adapters in the design: the shared state can be in Redis, a SQL database, or another store.
- Quality requirements: a coverage minimum of 98% for each module and 95% for each package, enforced in continuous integration.
- Continuous integration with build, `go vet`, static analysis, and tests.
- Requirements and architecture for the toolkit and its seven policies.
- Design documents, decision records, and roadmap for the circuit breaker.
