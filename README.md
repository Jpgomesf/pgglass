# mcp-postgres

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server that gives AI assistants read-only access to PostgreSQL databases. Built for [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and any MCP-compatible client.

## Features

- **Read-only by default** — all queries run inside `READ ONLY` transactions with blocked `INSERT`/`UPDATE`/`DELETE`/`DROP`/`ALTER`/`TRUNCATE`/`CREATE` statements.
- **Flexible credential sources** — connect via direct DSN, environment variables, `.env` files, or AWS Secrets Manager. Extensible via the `CredentialSource` interface.
- **Connection pooling** — connections are cached with a configurable TTL and validated with a ping before reuse.
- **Schema discovery** — inspect tables, columns, types, and constraints before querying.
- **Large result handling** — results exceeding 10K characters are written to a temp file with a summary returned inline.
- **VPN connectivity check** — optional TCP reachability check before connecting to remote hosts.

## Parallel Queries

Claude Code (and other MCP clients that support async tool calls) can issue multiple `pg_query` or `pg_schema` calls **in parallel**. Each call uses its own connection from the pool, so you can query multiple databases simultaneously. For example, Claude can inspect schema on three databases at the same time, or run a query on `staging` and `production` concurrently to compare results.

This is particularly effective for:
- Comparing data across databases or schemas
- Running independent queries that don't depend on each other
- Exploring multiple connections during investigation

## Available Tools

| Tool | Description |
|------|-------------|
| `pg_list_connections` | List all configured connections and their status |
| `pg_schema` | Discover tables and columns for a connection |
| `pg_query` | Execute a read-only SQL query |

## Quick Start

### 1. Build

```bash
make build
```

Or build manually:

```bash
go build -o mcp-postgres .
```

### 2. Configure

Create a `config.json` next to the binary (or in your working directory):

```json
{
  "defaults": {
    "readonly": true,
    "statement_timeout_seconds": 30
  },
  "connections": {
    "local": {
      "source": "direct",
      "dsn": "postgres://user:password@localhost:5432/mydb"
    }
  }
}
```

### 3. Add to Claude Code

Add to your `~/.claude.json` (global) or `.claude/settings.json` (per-project):

```json
{
  "mcpServers": {
    "mcp-postgres": {
      "type": "stdio",
      "command": "/path/to/mcp-postgres"
    }
  }
}
```

## Usage Guide

Once the server is running and connected to Claude Code, you interact with your databases through natural language. The AI assistant calls the MCP tools on your behalf.

### Step 1: Discover your connections

Ask Claude to list what's available:

> "What databases do I have access to?"

Claude calls `pg_list_connections` and returns something like:

```
Connections (3):
  local-dev [direct, readonly]
  staging-app [env, readonly, schema:app]
  prod-app [aws_secrets_manager, readonly, vpn, schema:app, loaded]
```

### Step 2: Explore the schema

Ask Claude to show you what's in a database:

> "Show me the tables in staging-app"

Claude calls `pg_schema` with `connection: "staging-app"`. You get back a tree of schemas, tables, and columns with types — enough context for Claude to write accurate queries.

You can also filter:

> "Show me just the users table in prod-app"

### Step 3: Query

Ask questions in plain language:

> "How many active users signed up this month in staging?"

Claude writes the SQL, calls `pg_query`, and returns formatted results. All queries are enforced read-only — Claude cannot modify your data even if it tries.

### Step 4: Cross-database investigation

This is where parallel queries shine. Ask something that spans databases:

> "Compare the row count of the orders table between staging and production"

Claude fires two `pg_query` calls **in parallel** — one against `staging-app`, one against `prod-app` — and presents the comparison. No waiting for sequential execution.

More examples:

> "Check if the users table schema matches between staging and prod"
>
> "Find all tables that have a column named 'tenant_id' across all my connections"
>
> "Show me the last 10 orders in prod and the matching records in the analytics db"

### Tips

- **Start with `pg_schema`** — Claude writes better queries when it knows the exact column names and types.
- **Use `default_schema`** in your config to avoid specifying the schema every time.
- **Large results** are automatically saved to a temp file with a summary. Claude can then read specific parts of the file instead of flooding the conversation.
- **Readonly is your friend** — keep it enabled in production. The server blocks writes at two levels: SQL keyword filtering and PostgreSQL's `default_transaction_read_only` mode.

## Connection Structure

### How Connections Work

Each entry in the `connections` map is a named, self-contained database connection. The connection name (the JSON key) is what you pass to `pg_query` and `pg_schema` — it must be unique and is used as the pool cache key.

```
config.json
  └── connections
        └── "my-db"                          ← connection name (used in tool calls)
              ├── source: "direct"           ← which CredentialSource resolves the DSN
              ├── dsn: "postgres://..."      ← source-specific fields
              ├── readonly: true             ← per-connection overrides
              └── default_schema: "public"   ← optional search_path
```

### DSN Formats

The server accepts two PostgreSQL DSN formats. Both are passed directly to `pgx`:

**URI format** (recommended for `direct`, `env`, `dotenv` sources):
```
postgres://user:password@host:5432/dbname?sslmode=require
```

**Keyword/value format** (used internally by the `aws_secrets_manager` source):
```
host=localhost port=5432 dbname=mydb user=admin password=secret
```

The server automatically appends `statement_timeout` and `default_transaction_read_only` parameters to whichever format you provide. URI-format DSNs get query parameters (`?key=value` or `&key=value`), keyword/value DSNs get space-separated pairs.

### AWS Secrets Manager Secret Structure

When using `aws_secrets_manager`, the secret value must be a JSON object with these fields:

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `host` | yes | — | Database hostname |
| `port` | no | `5432` | Database port |
| `db_name` | yes | — | Database name |
| `user` | yes | — | Database username |
| `password` | yes | — | Database password |

Example secret value:
```json
{
  "host": "mydb.cluster-abc123.us-east-1.rds.amazonaws.com",
  "port": "5432",
  "db_name": "myapp",
  "user": "readonly_user",
  "password": "secret"
}
```

Missing required fields (`host`, `db_name`, `user`, `password`) will produce a clear error message identifying which field is absent.

### Validation Rules

The server validates configuration at startup and at connection time:

| When | What | Error if invalid |
|------|------|-----------------|
| Startup | `source` must be a registered type | `unknown source "foo" (registered: direct, env, dotenv, aws_secrets_manager)` |
| Connect | `direct`: `dsn` must be non-empty | `'dsn' is required` |
| Connect | `env`: `dsn_env_var` must be set and the variable must exist | `environment variable "X" is not set` |
| Connect | `dotenv`: `dsn_env_var` must be set, file must exist, key must be present | `"X" not found in .env` |
| Connect | `aws_secrets_manager`: `secret_name` required, secret must contain required fields | `missing required field 'host'` |
| Connect | VPN check (if `requires_vpn: true`) | `host unreachable — is VPN active?` |

### Naming Conventions

Connection names are freeform strings but we recommend a consistent pattern:

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

Pattern: `{environment}-{database}`. This makes `pg_list_connections` output scannable and avoids ambiguity when you have the same database in multiple environments.

## Configuration Reference

### Credential Sources

#### Direct DSN

Inline connection string. Good for local development.

```json
{
  "source": "direct",
  "dsn": "postgres://user:password@localhost:5432/mydb"
}
```

#### Environment Variable

Read DSN from an environment variable at runtime.

```json
{
  "source": "env",
  "dsn_env_var": "DATABASE_URL"
}
```

#### Dotenv File

Read DSN from a `.env` file. Supports `KEY=VALUE`, quoted values, and comments.

```json
{
  "source": "dotenv",
  "dotenv_path": ".env",
  "dsn_env_var": "DATABASE_URL"
}
```

#### AWS Secrets Manager

Fetch credentials from AWS Secrets Manager. See [AWS Secrets Manager Secret Structure](#aws-secrets-manager-secret-structure) for the required JSON format.

```json
{
  "source": "aws_secrets_manager",
  "aws_profile": "my-profile",
  "secret_name": "prod/RDS/MyDatabase"
}
```

If `aws_profile` is omitted, the default AWS credential chain is used.

### Per-Connection Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `source` | string | *required* | `direct`, `env`, `dotenv`, or `aws_secrets_manager` |
| `readonly` | bool | from defaults | Override the global readonly setting |
| `requires_vpn` | bool | `false` | TCP reachability check before connecting |
| `default_schema` | string | `""` | Auto-set `search_path` when querying |

### Defaults

```json
{
  "defaults": {
    "readonly": true,
    "statement_timeout_seconds": 30
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `readonly` | `false` | Enforce `default_transaction_read_only=on` for all connections |
| `statement_timeout_seconds` | `30` | Max query execution time |

### Full Example

```json
{
  "defaults": {
    "readonly": true,
    "statement_timeout_seconds": 30
  },
  "connections": {
    "local-dev": {
      "source": "direct",
      "dsn": "postgres://dev:dev@localhost:5432/app_dev",
      "readonly": false,
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

## Extending with Custom Sources

The `CredentialSource` interface makes it straightforward to add new credential backends (e.g., HashiCorp Vault, GCP Secret Manager, 1Password CLI):

```go
type CredentialSource interface {
    // Type returns the source identifier used in config.json.
    Type() string

    // Resolve returns a libpq-compatible DSN string.
    Resolve(ctx context.Context, connName string, conn Connection) (string, error)
}
```

Register your implementation in an `init()` function:

```go
func init() {
    RegisterSource(&VaultSource{})
}
```

Then use it in `config.json`:

```json
{
  "source": "vault",
  "vault_path": "database/creds/readonly"
}
```

The server validates source types at startup, so an unregistered source will fail fast with a clear error listing all available types.

## Build & Test

```bash
make all        # lint + build
make test       # run tests with race detector
make build      # build binary only
make lint       # run golangci-lint
```

## Security

- All queries run inside read-only transactions by default.
- DDL and DML keywords are blocked at the tool layer before reaching the database.
- Statement timeout prevents runaway queries.
- Credentials are resolved at runtime and never logged.
- AWS secrets are cached in-memory with a 24-hour TTL (no disk persistence). Rotated credentials are picked up automatically.
- Direct DSN values in `config.json` should be treated as sensitive — use `env` or `dotenv` sources in shared environments, and keep `config.json` out of version control if it contains credentials.

## License

MIT
