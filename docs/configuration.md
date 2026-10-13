# Configuration

## Connection Structure

Each entry in the `connections` map is a named, self-contained database connection. The connection name (the JSON key) is what you pass to the tools as `connection`, or in `connections`, `left` and `right`.

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

Fetch credentials from AWS Secrets Manager. Each secret is cached in memory for 24 hours, per `aws_profile` and `secret_name`. When the server refuses the credentials of a new database connection (SQLSTATE `28P01` or `28000`), the secret is fetched again and the connection tried once more, so a rotated password needs no restart. Alternating-users rotation is different: it switches the secret to another user and leaves the cached user's password valid, so new database connections go on as the cached user until the cached secret expires, then fail until a restart, since the role check audited the first user (see [DSN Formats](#dsn-formats)). Single-user rotation keeps the user.

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

Whatever the DSN says, every session asks at startup for `default_transaction_read_only=on` and `client_encoding=UTF8`, and each query runs in a READ ONLY transaction that sets `statement_timeout`, `lock_timeout` and `standard_conforming_strings=on` for itself. Behind PgBouncer, `ignore_startup_parameters` drops the former, and the READ ONLY transaction alone remains (see [PgBouncer](#pgbouncer)).

Each connection's DSN is resolved on first use and again for every new database connection, which takes its password and TLS settings from it: a rotated password needs no restart. A DSN that comes to name another user, database or host fails new database connections until the server restarts, since the [role check](#role-check) audited the first one. The rest of the DSN, such as `pool_max_conns`, is read on first use only.

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
| `default_schema` | string | `""` | `search_path` for `pg_query` and `pg_explain` when the call names no `schema`, and the schema `pg_schema` and `pg_profile` look a `table` up in. `net`, `aws_s3` and `aws_lambda`, whose functions the guard denies, are refused as the `search_path`: with one of them here, `pg_query` and `pg_explain` calls that name no `schema` fail |

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

Every field is optional: a missing or zero value takes the default. Numbers must be positive. The timeouts can be at most 2147483 seconds, since Postgres takes them as an int of milliseconds, and `spill_ttl_hours` at most 2562047.

| Field | Default | Description |
|-------|---------|-------------|
| `statement_timeout_seconds` | `30` | Max query execution time |
| `lock_timeout_seconds` | `5` | Max time a query waits for a lock |
| `max_rows` | `1000` | Rows a query returns before its result is truncated. A `pg_query` call can lower it with `max_rows`, never raise it. Also caps `pg_search` |
| `max_bytes` | `1048576` (1 MiB) | Bytes a query's result holds before it is truncated: each value counts its length plus 24, so empty and NULL values count too. A server message larger than `max_bytes` plus 1 MiB is not read: the connection is reset and, once the columns are described, the result is truncated there; before, the query fails |
| `spill_ttl_hours` | `24` | How long spill files of large results are kept. They live in `mcp-postgres` under the user cache directory (or `mcp-postgres-<uid>` in the temp directory without one), and cleanup removes every spill file there older than this, including those of other server instances that share the directory |
| `format` | `tsv` | Result format of `pg_query` by default, and of `pg_profile` and `pg_health`: `tsv`, `json` or `markdown` |
| `role_check` | `warn` | What to do when a connection's role can do more than read (see below) |
| `readonly` | — | Deprecated and ignored: the server is always read-only. Logs a warning at startup |

### Role check

On a connection's first use, the server audits the role it connects as, once, and keeps the report. `pg_list_connections` shows it. When the connection sets another role, with `options=-c role=...` in the DSN or `ALTER ROLE ... SET role`, the session user it logged in as is audited too, since `RESET ROLE` returns to it; those findings start with `session user <name>: `.

| `role_check` | Effect |
|--------------|--------|
| `strict` | A role with a critical finding is refused: every tool but `pg_list_connections` fails on the connection and says why. Warnings alone are shown once |
| `warn` | Findings are shown once, at the top of the first response on the connection |
| `off` | No audit |

Critical findings, each a way out of the read-only transaction: superuser; membership in a superuser role, which `SET ROLE` reaches; membership in `pg_execute_server_program` or `pg_write_server_files`; `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export` or `autoprewarm_*` (pg_prewarm), or on a function that writes in place: `heap_force_*` (pg_surgery), `pg_truncate_visibility_map` (pg_visibility) or `pg_log_standby_snapshot`. Warnings: `BYPASSRLS`, `CREATEROLE`, `CREATEDB`; membership in `pg_read_server_files` or `pg_signal_backend`; `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE` on a relation; `EXECUTE` on `lo_import`, `pg_read_file`, `pg_read_binary_file` or `pg_log_backend_memory_contexts`. Only objects in schemas the role can use count. `CREATE EXTENSION pg_prewarm` grants `EXECUTE` on `autoprewarm_dump_now` and `autoprewarm_start_worker`, which write a server file, to `PUBLIC`: revoke it, or `strict` refuses every role that can use the extension's schema.

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
| Connect | The DSN must keep the user, database and host it had on first use | `the DSN now names another user, database or host; restart the server to use it` |
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
