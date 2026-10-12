# Extending with Custom Sources

The `CredentialSource` interface makes it straightforward to add new credential backends (e.g., HashiCorp Vault, GCP Secret Manager, 1Password CLI).

## Interface

```go
type CredentialSource interface {
    // Type returns the source identifier used in config.json.
    Type() string

    // Resolve returns a libpq-compatible DSN string.
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
  "vault_path": "database/creds/readonly"
}
```

No changes to `resolver.go`, `main.go`, or any other source files are needed. The server validates source types at startup, so an unregistered source fails fast with a clear error.

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
