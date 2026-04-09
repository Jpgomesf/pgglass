# mcp-postgres

An [MCP](https://modelcontextprotocol.io) server that gives AI assistants read-only access to PostgreSQL databases.

## Exposed Tools

| Tool | Description |
|------|-------------|
| `pg_list_connections` | List all configured connections and their status |
| `pg_schema` | Discover tables, columns, and types for a connection |
| `pg_query` | Execute a read-only SQL query |

All tools support **parallel execution** — query multiple databases simultaneously.

## Install

### From source

```bash
git clone git@github.com:Jpgomesf/mcp-postgres.git
cd mcp-postgres
make build
```

### From release

Download the binary for your platform from [Releases](https://github.com/Jpgomesf/mcp-postgres/releases).

## Configure

Create `config.json` next to the binary:

```json
{
  "defaults": {
    "readonly": true,
    "statement_timeout_seconds": 30
  },
  "connections": {
    "my-db": {
      "source": "direct",
      "dsn": "postgres://user:password@localhost:5432/mydb"
    }
  }
}
```

Four credential sources are supported: `direct`, `env`, `dotenv`, and `aws_secrets_manager`. See [Configuration](docs/configuration.md) for details.

## Add to Claude Code

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

Add to `~/.claude.json` (global) or `.claude/settings.json` (per-project).

## Documentation

| Doc | Description |
|-----|-------------|
| [Usage Guide](docs/usage.md) | How to interact with the tools, parallel queries, tips |
| [Configuration](docs/configuration.md) | Credential sources, connection options, validation rules, naming conventions |
| [Extending](docs/extending.md) | How to add custom credential sources (Vault, GCP, etc.) |
| [Contributing](CONTRIBUTING.md) | Branch strategy, commit conventions, development workflow |

## Security

- Read-only transactions enforced by default
- DDL/DML keywords blocked before reaching the database
- Statement timeout prevents runaway queries
- Credentials resolved at runtime, never logged
- AWS secrets cached in-memory with 24h TTL

## License

MIT
