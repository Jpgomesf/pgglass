# Configuration

## Connection Structure

Each entry in the `connections` map is a named, self-contained database connection. The connection name (the JSON key) is what you pass to `pg_query` and `pg_schema`.

```
config.json
  └── connections
        └── "my-db"                          ← connection name (used in tool calls)
              ├── source: "direct"           ← which CredentialSource resolves the DSN
              ├── dsn: "postgres://..."      ← source-specific fields
              └── default_schema: "public"   ← optional search_path
```

## Credential Sources

### Direct DSN

Inline connection string. Good for local development.

```json
{
  "source": "direct",
  "dsn": "postgres://user:password@localhost:5432/mydb"
}
```

### Environment Variable

Read DSN from an environment variable at runtime.

```json
{
  "source": "env",
  "dsn_env_var": "DATABASE_URL"
}
```

### Dotenv File

Read DSN from a `.env` file. Supports `KEY=VALUE`, quoted values, and comments.

```json
{
  "source": "dotenv",
  "dotenv_path": ".env",
  "dsn_env_var": "DATABASE_URL"
}
```

### AWS Secrets Manager

Fetch credentials from AWS Secrets Manager. Secrets are cached in-memory with a 24-hour TTL.

```json
{
  "source": "aws_secrets_manager",
  "aws_profile": "my-profile",
  "secret_name": "prod/RDS/MyDatabase"
}
```

If `aws_profile` is omitted, the default AWS credential chain is used.

The secret value must be a JSON object:

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `host` | yes | — | Database hostname |
| `port` | no | `5432` | Database port |
| `db_name` | yes | — | Database name |
| `user` | yes | — | Database username |
| `password` | yes | — | Database password |

Example:
```json
{
  "host": "mydb.cluster-abc123.us-east-1.rds.amazonaws.com",
  "port": "5432",
  "db_name": "myapp",
  "user": "readonly_user",
  "password": "secret"
}
```

## DSN Formats

The server accepts two PostgreSQL DSN formats, both passed directly to `pgx`:

**URI format** (recommended for `direct`, `env`, `dotenv`):
```
postgres://user:password@host:5432/dbname?sslmode=require
```

**Keyword/value format** (used internally by `aws_secrets_manager`):
```
host=localhost port=5432 dbname=mydb user=admin password=secret
```

The server automatically appends `statement_timeout` and `default_transaction_read_only` parameters.

## Per-Connection Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `source` | string | *required* | `direct`, `env`, `dotenv`, or `aws_secrets_manager` |
| `readonly` | bool | — | Deprecated and ignored: the server is always read-only |
| `requires_vpn` | bool | `false` | TCP reachability check before connecting |
| `default_schema` | string | `""` | Auto-set `search_path` when querying |

## Defaults

```json
{
  "defaults": {
    "statement_timeout_seconds": 30,
    "lock_timeout_seconds": 5,
    "max_rows": 1000,
    "max_bytes": 1048576,
    "spill_ttl_hours": 24,
    "format": "tsv",
    "role_check": "warn"
  }
}
```

Every field is optional: a missing or zero value takes the default. Numbers must be positive.

| Field | Default | Description |
|-------|---------|-------------|
| `statement_timeout_seconds` | `30` | Max query execution time |
| `lock_timeout_seconds` | `5` | Max time a query waits for a lock |
| `max_rows` | `1000` | Rows returned before the result is truncated |
| `max_bytes` | `1048576` (1 MiB) | Result bytes returned before the result is truncated |
| `spill_ttl_hours` | `24` | How long large results spilled to files are kept |
| `format` | `tsv` | Result format: `tsv`, `json` or `markdown` |
| `role_check` | `warn` | When the connected role can do more than read: `strict` refuses, `warn` warns, `off` skips the check |
| `readonly` | — | Deprecated and ignored: the server is always read-only |

## Validation Rules

| When | What | Error |
|------|------|-------|
| Startup | `defaults` values must be valid | `defaults.max_rows must be positive, got -1` |
| Startup | `source` must be a registered type | `unknown source "foo" (registered: ...)` |
| Connect | `direct`: `dsn` must be non-empty | `missing required fields: dsn` |
| Connect | `env`: `dsn_env_var` must be set, variable must exist | `environment variable "X" is not set` |
| Connect | `dotenv`: file must exist, key must be present | `"X" not found in .env` |
| Connect | `aws_secrets_manager`: secret must contain required fields | `missing required fields: host, db_name` |
| Connect | VPN check (if `requires_vpn: true`) | `host unreachable — is VPN active?` |

## Naming Conventions

We recommend `{environment}-{database}`:

```json
{
  "connections": {
    "local-appdb": { "source": "direct", "..." : "..." },
    "staging-appdb": { "source": "env", "..." : "..." },
    "prod-appdb": { "source": "aws_secrets_manager", "..." : "..." },
    "prod-analytics": { "source": "aws_secrets_manager", "..." : "..." }
  }
}
```

## Full Example

```json
{
  "defaults": {
    "statement_timeout_seconds": 30
  },
  "connections": {
    "local-dev": {
      "source": "direct",
      "dsn": "postgres://dev:dev@localhost:5432/app_dev",
      "default_schema": "public"
    },
    "staging-app": {
      "source": "env",
      "dsn_env_var": "STAGING_DATABASE_URL",
      "default_schema": "app"
    },
    "prod-app": {
      "source": "aws_secrets_manager",
      "aws_profile": "prod",
      "secret_name": "prod/RDS/AppDB",
      "requires_vpn": true,
      "default_schema": "app"
    }
  }
}
```
