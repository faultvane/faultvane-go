# Security policy

## Supported versions

Faultvane has no release yet.
After the first release, this document lists the versions that receive security fixes.

## Report a vulnerability

Do not open a public issue for a vulnerability.
Use the private vulnerability reporting function of the repository host.
Include these items:

- The affected version or commit.
- The steps to reproduce the problem.
- The effect that you expect.

The maintainers confirm the report, prepare a fix, and publish an advisory with the fixed version.

## Security properties of the library

- The library supports TLS and authentication for Redis through the Redis client that the application supplies.
- The library does not store or read Redis credentials. The application owns the client configuration.
- The library does not log credentials, request payloads, authorization tokens, or other sensitive configuration.
- Redis keys contain only the namespace and the breaker name. Do not put personal data in these names.
