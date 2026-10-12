# mcp-postgres

An [MCP](https://modelcontextprotocol.io) server that gives AI agents read-only access to PostgreSQL databases, built to be pointed at production: the server, not the model, holds the line.

## Tools

| Tool | Description |
|------|-------------|
| `pg_list_connections` | List the configured connections and their role-check status |
| `pg_schema` | Describe a database step by step: schemas, then relations, then one relation in detail |
| `pg_search` | Find relations, columns and functions by name |
| `pg_query` | Run one read-only statement and return its rows as TSV, JSON or Markdown |

Calls run in parallel, across connections and on the same one. See the [usage guide](docs/usage.md).

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
    "statement_timeout_seconds": 30,
    "role_check": "strict"
  },
  "connections": {
    "my-db": {
      "source": "direct",
      "dsn": "postgres://readonly_user:password@localhost:5432/mydb"
    }
  }
}
```

Credentials come from `direct`, `env`, `dotenv` or `aws_secrets_manager`. See [Configuration](docs/configuration.md) for every setting.

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

## Safety model

The calling agent is treated as untrusted: instructions can reach it through the data it reads (prompt injection). These layers hold whatever SQL it sends.

1. **The database role is the real boundary.** The server never gives the agent more than the role it connects as, so connect as a role that can read only what the agent should see. Row-level security applies as usual.
2. **The role is checked.** On a connection's first use, the server audits its role, and its session user too when the connection sets another role, since `RESET ROLE` returns to it: superuser, or membership in a superuser role, which `SET ROLE` reaches; `BYPASSRLS`, `CREATEROLE`, `CREATEDB`; membership in `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files` or `pg_signal_backend`; write privileges on tables and views; `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export`, `lo_import`, `pg_read_file`, `pg_read_binary_file` and `pg_log_backend_memory_contexts`. With `role_check: strict`, a connection with a critical finding is refused. With `warn` (the default), the findings come with the first response and in `pg_list_connections`.
3. **SQL is parsed, not pattern-matched.** `pg_query` parses each statement with the Postgres parser (libpg_query) and accepts exactly one `SELECT` (with `VALUES`, `TABLE` and set operations), `EXPLAIN [ANALYZE]` of one, or `SHOW`. Everything else is rejected: DML, DDL, `SET`, `COPY`, `DO`, `CALL`, `MERGE`, `PREPARE`, `LOCK`, transaction control, data-modifying `WITH`, `SELECT INTO` and `FOR UPDATE`/`FOR SHARE`. So are calls, anywhere in the statement and however spelled (`(x).f` included), to a denylist of known functions that reach outside the transaction, run SQL passed as text or read a relation passed by name: `dblink*`, `pg_background*`, `lo_*`, server-file functions such as `pg_read_file`, functions that signal other backends such as `pg_terminate_backend` and `pg_log_backend_memory_contexts`, `set_config`, `nextval`/`setval`, advisory locks, replication-slot and backup functions, statistics resets, pgsql-http's request functions (`http`, `http_get`, `http_post`, ...), any function in the `net` (pg_net), `aws_s3` and `aws_lambda` schemas, `query_to_xml`, `table_to_xml` and others. So are references to the catalogs that disclose server configuration files or credentials: `pg_hba_file_rules`, `pg_ident_file_mappings`, `pg_file_settings`, `pg_authid`, `pg_shadow`, the user-mapping catalogs and views, and `pg_subscription`. A `schema` argument naming `net`, `aws_s3` or `aws_lambda` is refused too, so their functions cannot be called unqualified.
4. **Every statement runs read-only and is rolled back.** Each one runs alone, over the extended protocol, in a `READ ONLY` transaction that is always rolled back, with `statement_timeout` and `lock_timeout` set for that transaction only. Sessions start with `default_transaction_read_only=on`, which no DSN can turn off, and `client_encoding=UTF8`. `standard_conforming_strings` is forced on, so Postgres reads the SQL exactly as the parser did.
5. **Results are bounded.** Rows past `max_rows` or `max_bytes` are not read: the rest of the query is cancelled on the server. A result whose output would exceed 10 KB is written to a private CSV file (mode 0600) and summarized.
6. **Calls are audited.** Each tool call writes one JSON line to stderr: tool, connection, SQL hash and length, duration, rows, outcome and SQLSTATE. SQL text, parameters and values are never logged.

What these layers do not cover:

- The parser knows names, not what is behind them, and the denylist holds only known functions: an extension can add another way out. A function the role may execute that calls `dblink` or another escape internally, typically `SECURITY DEFINER`, acts outside the transaction, and a view over a denied catalog reads it. Grant `EXECUTE` and `SELECT` with that in mind; the role check flags only the functions listed above.
- Reading is the point: whatever the role can `SELECT`, the agent can read and repeat. That includes what Postgres shows the role about the server: superusers and members of `pg_read_all_settings` read every setting, `primary_conninfo` and `archive_command` included (`SHOW`, `current_setting`, `pg_settings`); superusers and members of `pg_read_all_stats` read other sessions' SQL (`pg_stat_activity`, `pg_stat_statements`); superusers read large objects (`pg_largeobject`).
- A superuser connection has no role boundary. The role check flags it as critical, so `strict` refuses it; under `warn`, the parser and the read-only transaction are all that stand between the agent and the server's files and programs, and the denied catalogs are not the only place secrets show: column statistics in `pg_stats` can hold password verifiers. Use `strict`, or a role that is not a superuser.
- A query may use the database's CPU and I/O, and hold `ACCESS SHARE` locks that delay DDL, until `statement_timeout` ends it.

## Documentation

| Doc | Description |
|-----|-------------|
| [Usage Guide](docs/usage.md) | Tools, arguments, formats, large results, errors |
| [Configuration](docs/configuration.md) | Credential sources, connection options, defaults, PgBouncer |
| [Extending](docs/extending.md) | How to add custom credential sources (Vault, GCP, etc.) |
| [Contributing](CONTRIBUTING.md) | Branch strategy, commit conventions, development workflow |

## License

MIT
