# Security policy

## Reporting a vulnerability

Report a vulnerability privately through [GitHub's private vulnerability reporting](https://github.com/Jpgomesf/pgglass/security/advisories/new), not in a public issue. Include the pgglass version, the PostgreSQL version and a way to reproduce it.

A way around the read-only guarantees is in scope: SQL that writes, acts outside its transaction or reaches a denied function or catalog, a role-check finding that is missed, output past its bounds, or an HTTP request that reaches the tools without passing its checks. [The safety model](docs/safety-model.md) says what pgglass guarantees and what it does not.

## Supported versions

Fixes go into the latest release.
