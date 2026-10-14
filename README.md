# pgglass

**Production behind glass: look at everything, touch nothing.**

pgglass is an [MCP](https://modelcontextprotocol.io) server that gives AI agents read-only access to PostgreSQL databases, built to be pointed at production: the server, not the model, holds the line.

- **Read-only, enforced by the server.** Every statement the agent sends is parsed with the Postgres parser and must be one `SELECT`, `EXPLAIN` of one or `SHOW`. It runs in a `READ ONLY` transaction that is always rolled back, under a statement timeout, and each connection's role is audited on first use. See the [safety model](#safety-model).
- **Many databases, one call.** `pg_query` runs a statement on up to 10 connections in parallel and merges their rows under a `connection` column. `pg_diff_schema` lists what differs in a schema between two connections, such as production and staging.
- **Frugal with tokens.** Output is compact text, TSV by default. Plans, column statistics and server health come back as a few summary lines, not raw dumps. A query result past 10 KB is written to a private CSV file and summarized, so the agent reads or greps the file instead of the rows. The eight tool definitions fit in 6 KB of JSON. The [token benchmark](docs/benchmark.md) puts numbers on this, and fails CI when they grow.

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

Calls run in parallel, across connections and on the same one. The server and each tool have a display title, and on connecting the server gives the agent short instructions on using the tools well. It serves over stdio, or over Streamable HTTP with `--http`, speaks MCP revisions 2024-11-05 to 2026-07-28 and settles on the newest the client speaks too. See the [usage guide](docs/usage.md).

Tables are also MCP resources, `pgglass://<connection>/<schema>/<table>`, read through the same role check, limits and audit log as the tools: in Claude Code, `@`-mention one to add it to a prompt as `pg_schema` describes it. See [resources](docs/usage.md#mention-tables-as-resources).

## Install

pgglass runs on macOS and Linux, amd64 and arm64, and supports PostgreSQL 15, 16, 17 and 18, which CI tests each. Windows is not supported: see [Platforms](docs/releasing.md#platforms).

### Install script

```bash
curl -fsSL https://raw.githubusercontent.com/Jpgomesf/pgglass/main/install.sh | sh
```

The script downloads the latest release for your platform, checks its SHA-256 against the release's `checksums.txt`, and installs `pgglass` in `~/.local/bin` only if they match. To pick a release or a directory, set `PGGLASS_VERSION` or `PGGLASS_INSTALL_DIR` for `sh`: `... | PGGLASS_VERSION=v1.0.0 sh`.

### Release archive

Download your platform's `pgglass_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from [Releases](https://github.com/Jpgomesf/pgglass/releases), verify the archive, then unpack the binary:

```bash
sha256sum --ignore-missing -c checksums.txt
gh attestation verify pgglass_1.0.0_linux_amd64.tar.gz --repo Jpgomesf/pgglass
tar -xzf pgglass_1.0.0_linux_amd64.tar.gz pgglass
```

`gh attestation verify` checks that the release workflow of this repository built the archive.

### Docker

The image `ghcr.io/jpgomesf/pgglass` (linux/amd64 and linux/arm64, distroless, non-root) serves stdio. Mount the directory holding `config.json` read-only:

```bash
docker run --rm -i --user "$(id -u):$(id -g)" \
  -v ~/.config/pgglass:/etc/pgglass:ro \
  ghcr.io/jpgomesf/pgglass --config /etc/pgglass/config.json
```

- Give your MCP client that command, as in `claude mcp add pgglass -- docker run --rm -i ...`, and pin a version with a tag such as `:1.0.0`.
- `--user` runs the server as you, so it can read a `config.json` only you can read.
- Credentials resolve inside the container: pass the variables that `env` sources read with `-e`, and keep `.env` files in the mounted directory.
- A result past 10 KB is written to a file inside the container, out of the agent's reach. To share these files, run `mkdir -p ~/.cache/pgglass` and add `-v "$HOME/.cache/pgglass:$HOME/.cache/pgglass" -e XDG_CACHE_HOME="$HOME/.cache"`: the directory has the same path on both sides, so the agent can open the path the server reports.

To serve Streamable HTTP instead:

```bash
export PGGLASS_HTTP_TOKEN="$(openssl rand -hex 32)"
docker run --rm -p 127.0.0.1:8411:8411 -e PGGLASS_HTTP_TOKEN --user "$(id -u):$(id -g)" \
  -v ~/.config/pgglass:/etc/pgglass:ro \
  ghcr.io/jpgomesf/pgglass --config /etc/pgglass/config.json --http 0.0.0.0:8411
```

In the container the server has to listen on every interface, which requires the token; publishing the port on `127.0.0.1` keeps it on this machine. Clients connect to `http://127.0.0.1:8411/mcp` with the header `Authorization: Bearer $PGGLASS_HTTP_TOKEN`.

### From source

```bash
go install github.com/jpgomesf/pgglass@latest
```

Or clone the repository and run `make build`.

## Configure

Create `~/.config/pgglass/config.json` (`$XDG_CONFIG_HOME/pgglass/config.json` when `XDG_CONFIG_HOME` is set), or pass another file with `--config`:

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

Credentials come from `direct`, `env`, `dotenv` or `aws_secrets_manager`. See [Configuration](docs/configuration.md) for every setting. Then check the config and each connection's role:

```
$ pgglass doctor
config /home/me/.config/pgglass/config.json (role_check strict)
ok my-db: role readonly_user: no findings
0 failed, 1 passed
```

It exits 1 when a connection's credentials do not resolve, its server does not answer, or its role has a way out of the read-only transaction (see [Doctor](docs/configuration.md#doctor)).

## Add to Claude Code

Over stdio, Claude Code starts the server itself:

```bash
claude mcp add --transport stdio --scope user pgglass -- /path/to/pgglass
```

Put `--config /path/to/config.json` after the binary for a config file elsewhere. Over Streamable HTTP, start the server, then add its URL:

```bash
pgglass --http :8411
claude mcp add --transport http --scope user pgglass http://127.0.0.1:8411/mcp
```

`--scope user` adds the server to every project; leave it out for the current project alone, or use `--scope project` to share it in the project's `.mcp.json`. Other MCP clients take the same command or URL. To serve beyond this machine, with a bearer token, see [Streamable HTTP](docs/configuration.md#streamable-http).

## Upgrading from mcp-postgres

pgglass was called mcp-postgres. The tools are the same, and it reads the same `config.json`.

- mcp-postgres read the `config.json` next to its binary, else the one in its working directory; pgglass reads neither. Move that file to `~/.config/pgglass/config.json` (or `$XDG_CONFIG_HOME/pgglass/config.json`), or pass its path with `--config`.
- A relative `dotenv_path`, and the default `.env`, still resolve against the server's working directory, which the MCP client picks, not against the directory of `config.json`. If a `.env` moves with `config.json`, set `dotenv_path` to its absolute path.
- [Install](#install) pgglass, then rename the server in your MCP client and point it at the new binary. In Claude Code, run `claude mcp remove mcp-postgres` and [add pgglass](#add-to-claude-code), or rename the `mcp-postgres` entry under `mcpServers` in `~/.claude.json` or `.mcp.json` to `pgglass` and update its `command`.
- Rename the permission rules in your Claude Code settings that name the server: `mcp__mcp-postgres` becomes `mcp__pgglass`, and `mcp__mcp-postgres__pg_query` becomes `mcp__pgglass__pg_query`.
- Spill files now go to `pgglass` under the user cache directory, so the old `mcp-postgres` directory there can be deleted.

## Safety model

The calling agent is treated as untrusted: instructions can reach it through the data it reads (prompt injection). These layers hold whatever SQL it sends.

1. **The database role is the real boundary.** The server never gives the agent more than the role it connects as, so connect as a role that can read only what the agent should see. Row-level security applies as usual.
2. **The role is checked.** On a connection's first use, the server audits its role, and its session user too when the connection sets another role, since `RESET ROLE` returns to it: superuser, or membership in a superuser role, which `SET ROLE` reaches; `BYPASSRLS`, `CREATEROLE`, `CREATEDB`; membership in `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files`, `pg_signal_backend` or `pg_signal_autovacuum_worker`; write privileges on tables and views; `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export`, `autoprewarm_*`, `heap_force_*`, `pg_truncate_visibility_map`, `pg_log_standby_snapshot`, `lo_import`, `pg_read_file`, `pg_read_binary_file`, `pg_log_backend_memory_contexts` and `pg_get_logical_snapshot_*`. Findings that reach outside the read-only transaction are critical: superuser, directly or through a superuser role; the program and file-write roles; and `EXECUTE` on `dblink*`, `pg_background*`, `http*`, `net.*`, `aws_s3.*`, `aws_lambda.*`, `lo_export`, `autoprewarm_*` (pg_prewarm grants it to `PUBLIC`) and the functions that write in place, `heap_force_*`, `pg_truncate_visibility_map` and `pg_log_standby_snapshot`. The rest are warnings: the read-only transaction refuses table writes, for one. With `role_check: strict`, a connection with a critical finding is refused. With `warn` (the default), the findings come with the first response and in `pg_list_connections`. The report holds until the server restarts: if a connection's DSN comes to name another user, database or host, its new database connections fail until then.
3. **SQL is parsed, not pattern-matched.** `pg_query` parses each statement with the Postgres parser (libpg_query) and accepts exactly one `SELECT` (with `VALUES`, `TABLE` and set operations), `EXPLAIN [ANALYZE]` of one, or `SHOW`. `pg_explain` accepts the `SELECT` alone, and checks the `EXPLAIN` it wraps around it too. Everything else is rejected: DML, DDL, `SET`, `COPY`, `DO`, `CALL`, `MERGE`, `PREPARE`, `LOCK`, transaction control, data-modifying `WITH`, `SELECT INTO` and `FOR UPDATE`/`FOR SHARE`. So are calls, anywhere in the statement and however spelled (`(x).f` included), to a denylist of known functions that reach outside the transaction, write in place, take a transaction id or seed `random()` where neither `READ ONLY` nor the rollback reaches, run SQL passed as text or read a relation passed by name: `dblink*`, `pg_background*`, `lo_*`, server-file functions such as `pg_read_file` and pg_logicalinspect's `pg_get_logical_snapshot_*`, functions that signal other backends such as `pg_terminate_backend`, `pg_log_*` (`pg_log_backend_memory_contexts` signals another backend, `pg_log_standby_snapshot` writes WAL), pg_surgery's `heap_force_*`, `pg_truncate_visibility_map`, pg_prewarm's `autoprewarm_*` (`autoprewarm_dump_now` writes a server file) and `pg_prewarm` (it reads a relation into shared buffers, evicting the pages other sessions use), pg_buffercache's `pg_buffercache_evict*` (they evict those pages, writing out the dirty ones), BRIN and GIN index maintenance (`brin_summarize_new_values`, `brin_summarize_range`, `brin_desummarize_range`, `gin_clean_pending_list`), `txid_current` and `pg_current_xact_id`, catalog writers, which take a transaction id too (`pg_import_system_collations`, `pg_restore_*`, `pg_clear_*`), `set_config`, `setseed`, `nextval`/`setval`, advisory locks, replication-slot and backup functions, statistics resets, pgsql-http's request functions (`http`, `http_get`, `http_post`, ...), any function in the `net` (pg_net), `aws_s3` and `aws_lambda` schemas, `query_to_xml`, `table_to_xml` and others. So are references to the catalogs that disclose server configuration files or credentials: `pg_hba_file_rules`, `pg_ident_file_mappings`, `pg_file_settings`, `pg_authid`, `pg_shadow`, the user-mapping catalogs and views, and `pg_subscription`. The parser sees only the names a statement spells, so a `schema` argument or `default_schema` naming `net`, `aws_s3` or `aws_lambda` is refused as the `search_path` too. With neither, the query runs under the session's own `search_path`, which the server does not check (`"$user", public` by default; a setting of the role, the database or the DSN can change it): if it names one of those schemas, their functions can be called unqualified. The other tools send only SQL the server writes, with the agent's names as bound parameters or, in `pg_profile`'s sample, as quoted identifiers; `pg_profile` refuses the catalogs above, whose statistics hold their values.
4. **Every statement runs read-only and is rolled back.** Each one runs alone, over the extended protocol, in a `READ ONLY` transaction that is always rolled back, with `statement_timeout` and `lock_timeout` set for that transaction only. Neither stops a C function that writes in place, such as `pg_log_standby_snapshot`'s WAL record, pg_surgery's `heap_force_kill` or `brin_summarize_new_values`, nor one that takes a transaction id for good, such as `txid_current`, or changes session state past the rollback, such as `setseed`'s seed for `random()`: the parser denies those by name. Sessions start with `default_transaction_read_only=on`, which no DSN can turn off, and `client_encoding=UTF8`; behind PgBouncer, listing the former in `ignore_startup_parameters` drops it (see [PgBouncer](docs/configuration.md#pgbouncer)), and the `READ ONLY` transaction alone remains. `standard_conforming_strings` is forced on, so Postgres reads the SQL exactly as the parser did.
5. **Results are bounded.** Rows past `max_rows` or `max_bytes` are not kept, and the rest of the query is cancelled on the server; `max_bytes` counts each value's length plus 24 bytes, so empty and NULL values count too. A single server message larger than `max_bytes` plus 1 MiB is refused unread and resets the connection: once the columns are described, the result is truncated there; before, the query fails. A result whose output would exceed 10 KB is written to a private CSV file (mode 0600) in `pgglass` under your user cache directory or, without one, in `pgglass-<uid>` in the temp directory, which must be yours alone (mode 0700), and summarized. No tool result or resource read exceeds 10 KB, errors and the role check's findings included: the findings take at most half, and an error too long for the rest is cut in the middle. `pg_schema` reads at most 500 schemas or relations per listing from the catalog, and at most 200 characters of each comment; its output past 10 KB is cut at a line, with a note that counts them all. `pg_diff_schema` reads each side's schema as one catalog value, which `max_bytes` bounds too: the default 1 MiB holds about 1,600 tables of 7 columns, and a larger schema fails, naming its side, until `max_bytes` is raised.
6. **Calls are audited.** Each tool call and resource read writes one JSON line to stderr, one per connection when `pg_query` fans out or the resources are listed: tool, connection (`left,right` for `pg_diff_schema`), SQL hash and length, duration, rows, outcome and SQLSTATE. SQL text, parameters and values are never logged.
7. **HTTP stays on this machine unless told otherwise.** `--http` listens on loopback by default, where any local process can call it. Any other address needs a bearer token, `PGGLASS_HTTP_TOKEN`, that every request must then carry; set it on loopback too on a machine others use. A request from a web page on another origin, or to a loopback address under another host name (DNS rebinding), is refused, so a site open in a browser cannot call the tools.
8. **A connection can ask first.** With `confirm` set, `pg_query`, and `pg_explain` with `analyze`, show the user the statement, its `params` and the `search_path` it runs under through the client, and run only if they accept; a client that cannot ask is refused (see [Confirmation](docs/usage.md#confirmation)).

What these layers do not cover:

- The parser knows names, not what is behind them, and the denylist holds only known functions: an extension can add another way out. A function the role may execute that calls `dblink` or another escape internally, typically `SECURITY DEFINER`, acts outside the transaction, and a view over a denied catalog reads it. Grant `EXECUTE` and `SELECT` with that in mind; the role check flags only the functions listed above.
- Reading is the point: whatever the role can `SELECT`, the agent can read and repeat. That includes what Postgres shows the role about the server: superusers and members of `pg_read_all_settings` read every setting, `primary_conninfo` and `archive_command` included (`SHOW`, `current_setting`, `pg_settings`); superusers and members of `pg_read_all_stats` read other sessions' SQL (`pg_stat_activity`, `pg_stat_statements`); superusers read large objects (`pg_largeobject`).
- A superuser connection has no role boundary. The role check flags it as critical, so `strict` refuses it; under `warn`, the parser and the read-only transaction are all that stand between the agent and the server's files and programs, and against functions that write in place, such as pg_surgery's, the parser's denylist alone. The denied catalogs are not the only place secrets show either: column statistics in `pg_stats` can hold password verifiers. Use `strict`, or a role that is not a superuser.
- A query may use the database's CPU and I/O, and hold `ACCESS SHARE` locks that delay DDL, until `statement_timeout` ends it.
- `--http` serves plain HTTP. To reach it from elsewhere, put a proxy that terminates TLS in front of it, and set `PGGLASS_HTTP_TOKEN` even when pgglass listens on loopback behind the proxy, whose requests it cannot tell from local ones.

## Documentation

| Doc | Description |
|-----|-------------|
| [Usage Guide](docs/usage.md) | Tools, arguments, formats, large results, errors |
| [Configuration](docs/configuration.md) | Credential sources, connection options, defaults, PgBouncer, command line, doctor, Streamable HTTP |
| [Extending](docs/extending.md) | How to add custom credential sources (Vault, GCP, etc.) |
| [Contributing](CONTRIBUTING.md) | Branch strategy, commit conventions, development workflow |
| [Token benchmark](docs/benchmark.md) | What pgglass puts in an agent's context, measured, and how other servers' tool lists compare |
| [Releasing](docs/releasing.md) | What a release publishes, cutting one, the MCP Registry, platforms |

## License

[MIT](LICENSE)
