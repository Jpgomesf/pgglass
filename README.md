# mcp-postgres

An [MCP](https://modelcontextprotocol.io) server that gives AI agents read-only access to PostgreSQL databases, built to be pointed at production: the server, not the model, holds the line.

- **Read-only, enforced by the server.** Every statement the agent sends is parsed with the Postgres parser and must be one `SELECT`, `EXPLAIN` of one or `SHOW`. It runs in a `READ ONLY` transaction that is always rolled back, under a statement timeout, and each connection's role is audited on first use. See the [safety model](#safety-model).
- **Many databases, one call.** `pg_query` runs a statement on up to 10 connections in parallel and merges their rows under a `connection` column. `pg_diff_schema` lists what differs in a schema between two connections, such as production and staging.
- **Frugal with tokens.** Output is compact text, TSV by default. Plans, column statistics and server health come back as a few summary lines, not raw dumps. A query result past 10 KB is written to a private CSV file and summarized, so the agent reads or greps the file instead of the rows. The eight tool definitions fit in 6 KB of JSON.

## Tools

| Tool | Description |
|------|-------------|
| `pg_list_connections` | List the configured connections and their role-check status |
| `pg_schema` | Describe a database step by step: schemas, then relations, then one relation in detail |
| `pg_search` | Find relations, columns and functions by name |
| `pg_query` | Run one read-only statement on one connection, or on up to 10 in parallel, and return its rows as TSV, JSON or Markdown |
| `pg_explain` | Summarize the plan of a `SELECT`, with `EXPLAIN ANALYZE` on request, and flag large sequential scans, misestimates, spills to disk and large nested loops |
| `pg_profile` | Profile a table's columns from `pg_stats`: nulls, distinct values, most common values, range; when `pg_stats` has nothing on them, from a sample of at most 10,000 rows |
| `pg_diff_schema` | Compare one schema on two connections: relations, view queries, columns, constraints, indexes and RLS policies |
| `pg_health` | Check a server: connections, long-running and blocked sessions, cache hit ratio, dead rows, unused indexes, transaction ID wraparound and replica lag |

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
2. **The role is checked.** On a connection's first use, the server audits its role, and its session user too when the connection sets another role, since `RESET ROLE` returns to it: superuser, or membership in a superuser role, which `SET ROLE` reaches; `BYPASSRLS`, `CREATEROLE`, `CREATEDB`; membership in `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files` or `pg_signal_backend`; write privileges on tables and views; `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export`, `autoprewarm_*`, `heap_force_*`, `pg_truncate_visibility_map`, `pg_log_standby_snapshot`, `lo_import`, `pg_read_file`, `pg_read_binary_file` and `pg_log_backend_memory_contexts`. Findings that reach outside the read-only transaction are critical: superuser, directly or through a superuser role; the program and file-write roles; and `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export`, `autoprewarm_*` (pg_prewarm grants it to `PUBLIC`) and the functions that write in place, `heap_force_*`, `pg_truncate_visibility_map` and `pg_log_standby_snapshot`. The rest are warnings: the read-only transaction refuses table writes, for one. With `role_check: strict`, a connection with a critical finding is refused. With `warn` (the default), the findings come with the first response and in `pg_list_connections`. The report holds until the server restarts: if a connection's DSN comes to name another user, database or host, its new database connections fail until then.
3. **SQL is parsed, not pattern-matched.** `pg_query` parses each statement with the Postgres parser (libpg_query) and accepts exactly one `SELECT` (with `VALUES`, `TABLE` and set operations), `EXPLAIN [ANALYZE]` of one, or `SHOW`. `pg_explain` accepts the `SELECT` alone, and checks the `EXPLAIN` it wraps around it too. Everything else is rejected: DML, DDL, `SET`, `COPY`, `DO`, `CALL`, `MERGE`, `PREPARE`, `LOCK`, transaction control, data-modifying `WITH`, `SELECT INTO` and `FOR UPDATE`/`FOR SHARE`. So are calls, anywhere in the statement and however spelled (`(x).f` included), to a denylist of known functions that reach outside the transaction, write in place, take a transaction id or seed `random()` where neither `READ ONLY` nor the rollback reaches, run SQL passed as text or read a relation passed by name: `dblink*`, `pg_background*`, `lo_*`, server-file functions such as `pg_read_file`, functions that signal other backends such as `pg_terminate_backend`, `pg_log_*` (`pg_log_backend_memory_contexts` signals another backend, `pg_log_standby_snapshot` writes WAL), pg_surgery's `heap_force_*`, `pg_truncate_visibility_map`, pg_prewarm's `autoprewarm_*` (`autoprewarm_dump_now` writes a server file) and `pg_prewarm` (it reads a relation into shared buffers, evicting the pages other sessions use), BRIN and GIN index maintenance (`brin_summarize_new_values`, `brin_summarize_range`, `brin_desummarize_range`, `gin_clean_pending_list`), `txid_current` and `pg_current_xact_id`, catalog writers, which take a transaction id too (`pg_import_system_collations`, `pg_restore_*`, `pg_clear_*`), `set_config`, `setseed`, `nextval`/`setval`, advisory locks, replication-slot and backup functions, statistics resets, pgsql-http's request functions (`http`, `http_get`, `http_post`, ...), any function in the `net` (pg_net), `aws_s3` and `aws_lambda` schemas, `query_to_xml`, `table_to_xml` and others. So are references to the catalogs that disclose server configuration files or credentials: `pg_hba_file_rules`, `pg_ident_file_mappings`, `pg_file_settings`, `pg_authid`, `pg_shadow`, the user-mapping catalogs and views, and `pg_subscription`. The parser sees only the names a statement spells, so a `schema` argument or `default_schema` naming `net`, `aws_s3` or `aws_lambda` is refused as the `search_path` too. With neither, the query runs under the session's own `search_path`, which the server does not check (`"$user", public` by default; a setting of the role, the database or the DSN can change it): if it names one of those schemas, their functions can be called unqualified. The other tools send only SQL the server writes, with the agent's names as bound parameters or, in `pg_profile`'s sample, as quoted identifiers; `pg_profile` refuses the catalogs above, whose statistics hold their values.
4. **Every statement runs read-only and is rolled back.** Each one runs alone, over the extended protocol, in a `READ ONLY` transaction that is always rolled back, with `statement_timeout` and `lock_timeout` set for that transaction only. Neither stops a C function that writes in place, such as `pg_log_standby_snapshot`'s WAL record, pg_surgery's `heap_force_kill` or `brin_summarize_new_values`, nor one that takes a transaction id for good, such as `txid_current`, or changes session state past the rollback, such as `setseed`'s seed for `random()`: the parser denies those by name. Sessions start with `default_transaction_read_only=on`, which no DSN can turn off, and `client_encoding=UTF8`; behind PgBouncer, listing the former in `ignore_startup_parameters` drops it (see [PgBouncer](docs/configuration.md#pgbouncer)), and the `READ ONLY` transaction alone remains. `standard_conforming_strings` is forced on, so Postgres reads the SQL exactly as the parser did.
5. **Results are bounded.** Rows past `max_rows` or `max_bytes` are not kept, and the rest of the query is cancelled on the server; `max_bytes` counts each value's length plus 24 bytes, so empty and NULL values count too. A single server message larger than `max_bytes` plus 1 MiB is refused unread and resets the connection: once the columns are described, the result is truncated there; before, the query fails. A result whose output would exceed 10 KB is written to a private CSV file (mode 0600) in `mcp-postgres` under your user cache directory or, without one, in `mcp-postgres-<uid>` in the temp directory, which must be yours alone (mode 0700), and summarized. No response exceeds 10 KB, errors and the role check's findings included: the findings take at most half, and an error too long for the rest is cut in the middle. `pg_schema` reads at most 500 schemas or relations per listing from the catalog, and its output past 10 KB is cut at a line, with a note that counts them all. `pg_diff_schema` reads each side's schema as one catalog value, which `max_bytes` bounds too: the default 1 MiB holds about 1,600 tables of 7 columns, and a larger schema fails, naming its side, until `max_bytes` is raised.
6. **Calls are audited.** Each tool call writes one JSON line to stderr, one per connection when `pg_query` fans out: tool, connection (`left,right` for `pg_diff_schema`), SQL hash and length, duration, rows, outcome and SQLSTATE. SQL text, parameters and values are never logged.

What these layers do not cover:

- The parser knows names, not what is behind them, and the denylist holds only known functions: an extension can add another way out. A function the role may execute that calls `dblink` or another escape internally, typically `SECURITY DEFINER`, acts outside the transaction, and a view over a denied catalog reads it. Grant `EXECUTE` and `SELECT` with that in mind; the role check flags only the functions listed above.
- Reading is the point: whatever the role can `SELECT`, the agent can read and repeat. That includes what Postgres shows the role about the server: superusers and members of `pg_read_all_settings` read every setting, `primary_conninfo` and `archive_command` included (`SHOW`, `current_setting`, `pg_settings`); superusers and members of `pg_read_all_stats` read other sessions' SQL (`pg_stat_activity`, `pg_stat_statements`); superusers read large objects (`pg_largeobject`).
- A superuser connection has no role boundary. The role check flags it as critical, so `strict` refuses it; under `warn`, the parser and the read-only transaction are all that stand between the agent and the server's files and programs, and against functions that write in place, such as pg_surgery's, the parser's denylist alone. The denied catalogs are not the only place secrets show either: column statistics in `pg_stats` can hold password verifiers. Use `strict`, or a role that is not a superuser.
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
