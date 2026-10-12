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

**URI format** (recommended for `direct`, `env`, `dotenv`; `aws_secrets_manager` builds one from the secret, escaping each value):
```
postgres://user:password@host:5432/dbname?sslmode=require
```

**Keyword/value format**:
```
host=localhost port=5432 dbname=mydb user=admin password=secret
```

Whatever the DSN says, every session starts with `default_transaction_read_only=on` and `client_encoding=UTF8`, and each query sets `statement_timeout`, `lock_timeout` and `standard_conforming_strings=on` for its own transaction.

Each connection's DSN is resolved once, on first use, and kept for the life of the process: restart the server after rotating a password or secret.

### PgBouncer

PgBouncer refuses startup parameters it does not know, and with its default settings it rejects `default_transaction_read_only` (error `08P01`). Add the parameter to `track_extra_parameters` (PgBouncer 1.20+), which keeps it for each client, or to `ignore_startup_parameters`, which drops it; each query is still READ ONLY either way. Transaction pooling works: queries keep no state outside their transaction.

```ini
track_extra_parameters = default_transaction_read_only
```

## Per-Connection Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `source` | string | *required* | `direct`, `env`, `dotenv`, or `aws_secrets_manager` |
| `readonly` | bool | — | Deprecated and ignored: the server is always read-only. Logs a warning at startup |
| `requires_vpn` | bool | `false` | TCP reachability check before each new database connection |
| `default_schema` | string | `""` | `search_path` for `pg_query` when the call names no `schema`, and the schema `pg_schema` looks a `table` up in |

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
| `max_rows` | `1000` | Rows a query returns before its result is truncated. A `pg_query` call can lower it with `max_rows`, never raise it. Also caps `pg_search` |
| `max_bytes` | `1048576` (1 MiB) | Bytes of values a query returns before its result is truncated |
| `spill_ttl_hours` | `24` | How long spill files of large results are kept |
| `format` | `tsv` | Default result format of `pg_query`: `tsv`, `json` or `markdown` |
| `role_check` | `warn` | What to do when a connection's role can do more than read (see below) |
| `readonly` | — | Deprecated and ignored: the server is always read-only. Logs a warning at startup |

### Role check

On a connection's first use, the server audits the role it connects as, once, and keeps the report. `pg_list_connections` shows it.

| `role_check` | Effect |
|--------------|--------|
| `strict` | A role with a critical finding is refused: `pg_query`, `pg_schema` and `pg_search` on the connection fail and say why. Warnings alone are shown once |
| `warn` | Findings are shown once, at the top of the first response on the connection |
| `off` | No audit |

Critical findings: superuser; membership in `pg_execute_server_program` or `pg_write_server_files`; `EXECUTE` on `dblink*`, `http*`, `net.*` or `lo_export`. Warnings: `BYPASSRLS`, `CREATEROLE`, `CREATEDB`; membership in `pg_read_server_files` or `pg_signal_backend`; `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE` on a relation; `EXECUTE` on `lo_import` or `pg_read_file`. Only objects in schemas the role can use count.

If the audit itself fails, it is retried on the next call; meanwhile `strict` refuses the connection and `warn` lets calls through with a note.

## Validation Rules

| When | What | Error |
|------|------|-------|
| Startup | Every key must be known: a misspelled one is not ignored | `json: unknown field "role_chek"` |
| Startup | `defaults` values must be valid | `defaults.max_rows must be positive, got -1` |
| Startup | `source` must be a registered type | `unknown source "foo" (registered: ...)` |
| Connect | `direct`: `dsn` must be non-empty | `missing required fields: dsn` |
| Connect | `env`: `dsn_env_var` must be set, variable must exist | `environment variable "X" is not set` |
| Connect | `dotenv`: file must exist, key must be present | `"X" not found in .env` |
| Connect | `aws_secrets_manager`: secret must contain required fields | `missing required fields: host, db_name` |
| Connect | VPN check (if `requires_vpn: true`) | `host unreachable — is VPN active?` |
| First use | `role_check: strict` and a critical finding | `connection "x" refused (role_check strict): its role can do more than read.` |

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
    "statement_timeout_seconds": 30,
    "role_check": "strict"
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
