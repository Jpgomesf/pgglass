# Contributing to mcp-postgres

Thanks for your interest in contributing! This document covers the workflow and guidelines for getting your changes merged.

## Getting Started

1. **Fork** the repository
2. **Clone** your fork locally
3. **Build** and verify tests pass:
   ```bash
   make all
   make test
   ```

## Branch Strategy

We use a simple trunk-based workflow:

- **`main`** — stable, release-ready code. All changes go through pull requests.
- **Feature branches** — created from `main`, merged back via PR. Use descriptive names:
  - `feat/add-vault-source`
  - `fix/cache-ttl-race`
  - `docs/improve-aws-setup`
  - `refactor/pool-cleanup`

There is no `develop` branch. Keep it simple.

## Making Changes

### 1. Create a branch

```bash
git checkout -b feat/my-feature main
```

### 2. Write your code

- Follow the existing code style and package structure
- Keep changes focused — one concern per PR
- Add or update tests for any new functionality
- Run `make lint` and `make test` before committing

### 3. Commit conventions

We use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add HashiCorp Vault credential source
fix: handle expired AWS sessions gracefully
refactor: extract DSN builder from pool
docs: clarify dotenv file format
test: add validator edge cases
chore: update dependencies
```

- Keep the subject line under 72 characters
- Use the body for context when the "why" isn't obvious
- One logical change per commit — don't bundle unrelated changes

### 4. Open a pull request

- Target the `main` branch
- Write a clear description of what changed and why
- Link any related issues
- Ensure CI passes (lint + tests)

## Adding a New Credential Source

This is the most common type of contribution. The process:

1. Create `internal/resolver/source_yourname.go`
2. Implement the `CredentialSource` interface:
   ```go
   type CredentialSource interface {
       Type() string
       Resolve(ctx context.Context, connName string, conn config.Connection) (string, error)
       Status(conn config.Connection) string
   }
   ```
3. Self-register in `init()`:
   ```go
   func init() { RegisterSource(&YourSource{}) }
   ```
4. If your source needs new fields on `Connection`, add them to `internal/config/config.go`
5. Add tests in `internal/resolver/source_yourname_test.go`
6. Document the source in `README.md` under Configuration Reference

No changes to `resolver.go`, `main.go`, or any other source files should be needed.

## Project Structure

```
├── main.go                              # entrypoint and wiring
├── config.json                          # example configuration
├── internal/
│   ├── config/                          # types, constants, config loader
│   ├── resolver/                        # CredentialSource interface + implementations
│   ├── pool/                            # connection pool, DSN helpers
│   ├── output/                          # result formatting
│   └── tools/                           # MCP tool handlers
```

- `internal/` prevents external import — all packages are internal by design
- Tests live alongside source files (`foo.go` → `foo_test.go`)
- Each credential source is a single self-contained file

## Code Guidelines

- **Keep it simple** — prefer the straightforward solution over the clever one
- **Type hints everywhere** — Go's type system is your friend
- **No global state** — dependencies are injected via structs
- **Validate early** — use `FieldValidator` for checking required fields
- **Read-only by default** — never add write capabilities to the query layer

## Running Tests

```bash
make test          # all tests with race detector
go test ./...      # quick run without race detector
go test ./internal/resolver/...  # single package
```

## Releases

Releases are automated via GitHub Actions and GoReleaser. To create a release:

```bash
git tag v1.2.0
git push origin v1.2.0
```

Binaries for Linux (amd64) and macOS (arm64) are built and published automatically.

## Questions?

Open an issue for bugs, feature requests, or questions. We're happy to help contributors get started.
