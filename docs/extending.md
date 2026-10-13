# Extending with Custom Sources

The `CredentialSource` interface makes it straightforward to add new credential backends (e.g., HashiCorp Vault, GCP Secret Manager, 1Password CLI).

## Interface

```go
type CredentialSource interface {
    // Type returns the source identifier used in config.json.
    Type() string

    // Resolve returns a libpq-compatible DSN string. It runs when a connection
    // is first used and again for each new database connection.
    Resolve(ctx context.Context, connName string, conn config.Connection) (string, error)

    // Status returns a short hint for pg_list_connections (e.g., "loaded").
    // Return "" if the source has no meaningful status.
    Status(conn config.Connection) string
}
```

## Adding a Source

1. Create `internal/resolver/source_yourname.go`
2. Implement the interface
3. Self-register in `init()`

```go
package resolver

import (
    "context"
    "github.com/jpgomesf/mcp-postgres/internal/config"
)

func init() { RegisterSource(&VaultSource{}) }

type VaultSource struct{}

func (VaultSource) Type() string { return "vault" }

func (VaultSource) Resolve(ctx context.Context, connName string, conn config.Connection) (string, error) {
    // Your logic here — return a DSN string
}

func (VaultSource) Status(conn config.Connection) string { return "" }
```

4. If your source needs new fields, add them to `Connection` in `internal/config/config.go`: `config.json` rejects unknown keys
5. Add tests in `internal/resolver/source_yourname_test.go`
6. Use it in `config.json`:

```json
{
  "source": "vault",
  "vault_path": "database/static-creds/readonly"
}
```

No changes to `resolver.go`, `main.go`, or any other source files are needed. The server validates source types at startup, so an unregistered source fails fast with a clear error.

Since `Resolve` runs for each new database connection, a source can return rotated passwords, such as those of a Vault static role under `database/static-creds/readonly`. Cache them while they are valid, as the AWS source caches its secrets, and implement `resolver.Invalidator`: when the server refuses the credentials of a new database connection, the engine calls `Invalidate(conn)` and tries once more, so a rotated password reaches the cache. The user, database and host must stay the same, since the role check audits a connection once: a DSN that names others fails new database connections until the server restarts. So prefer a Vault static role, whose user stays, to dynamic `database/creds/...` leases, which each bring a new user.

## Validation

Use `FieldValidator` for consistent error reporting:

```go
v := NewValidator(connName, "vault")
v.Require("vault_path", conn.VaultPath)
if err := v.Err(); err != nil {
    return "", err
}
```

This collects all missing fields and reports them in a single error message.
