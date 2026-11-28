# Security policy

## Supported versions

Only the latest release receives security fixes.

| Version | Supported |
|---|---|
| 0.3.x | Yes |
| 0.2.x | No |
| 0.1.x | No |

## Report a vulnerability

Do not open a public issue for a vulnerability.
Use the private vulnerability reporting function of the repository host.
Include these items:

- The affected version or commit.
- The steps to reproduce the problem.
- The effect that you expect.

The maintainers confirm the report, prepare a fix, and publish an advisory with the fixed version.

## Security properties of the library

- The library supports TLS and authentication for the shared store through the client that the application supplies, for example the Redis client or the database connection.
- The library does not keep or read credentials of the store. The application owns the client configuration.
- The library does not log credentials, request payloads, authorization tokens, or other sensitive configuration.
- The records in the store contain only the namespace and the breaker name. Do not put personal data in these names.
