# mcp-postgres

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server that gives AI assistants read-only access to PostgreSQL databases. Built for [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and any MCP-compatible client.

## Features

- **Read-only by default** — all queries run inside `READ ONLY` transactions with blocked `INSERT`/`UPDATE`/`DELETE`/`DROP`/`ALTER`/`TRUNCATE`/`CREATE` statements.
- **Flexible credential sources** — connect via direct DSN, environment variables, `.env` files, or AWS Secrets Manager.
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

Fetch credentials from AWS Secrets Manager. The secret must be a JSON object with keys: `host`, `port` (optional, defaults to `5432`), `db_name`, `user`, `password`.

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
    "staging": {
      "source": "env",
      "dsn_env_var": "STAGING_DATABASE_URL",
      "default_schema": "app"
    },
    "production": {
      "source": "aws_secrets_manager",
      "aws_profile": "prod",
      "secret_name": "prod/RDS/AppDB",
      "requires_vpn": true,
      "default_schema": "app"
    }
  }
}
```

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
- AWS secrets are cached in-memory for the lifetime of the process (no disk persistence).
- Direct DSN values in `config.json` should be treated as sensitive — use `env` or `dotenv` sources in shared environments, and keep `config.json` out of version control if it contains credentials.

## License

MIT
