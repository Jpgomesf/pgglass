<h1 align="center">pgglass</h1>

<p align="center"><strong>Production behind glass: look at everything, touch nothing.</strong></p>

<p align="center">An <a href="https://modelcontextprotocol.io">MCP</a> server that gives AI agents read-only access to PostgreSQL, built to be pointed at production: the server, not the model, holds the line.</p>

<p align="center">
  <a href="https://github.com/Jpgomesf/pgglass/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Jpgomesf/pgglass/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/Jpgomesf/pgglass/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/Jpgomesf/pgglass"></a>
  <a href="https://pkg.go.dev/github.com/jpgomesf/pgglass"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/jpgomesf/pgglass.svg"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue"></a>
  <img alt="PostgreSQL 15–18" src="https://img.shields.io/badge/PostgreSQL-15%E2%80%9318-336791?logo=postgresql&logoColor=white">
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="#tools">Tools</a> ·
  <a href="#many-databases-one-call">Many databases</a> ·
  <a href="#how-it-compares">How it compares</a> ·
  <a href="#safety-model">Safety model</a> ·
  <a href="#install">Install</a> ·
  <a href="#documentation">Docs</a>
</p>

<picture><source media="(prefers-color-scheme: dark)" srcset="docs/assets/diagrams/safety-layers-dark.png"><img alt="An agent's untrusted SQL passes the Postgres parser, the role check and a READ ONLY transaction that is always rolled back before it reaches the database role; results are bounded and every call is audited." src="docs/assets/diagrams/safety-layers.png" width="830"></picture>

- **Read-only, enforced by the server.** Every statement the agent sends is parsed with the Postgres parser and must be one `SELECT`, `EXPLAIN` of one or `SHOW`. It runs in a `READ ONLY` transaction that is always rolled back, under a statement timeout, and each connection's role is audited on first use. See the [safety model](#safety-model).
- **Many databases, one call.** `pg_query` runs a statement on up to 10 connections in parallel and merges their rows under a `connection` column. `pg_diff_schema` lists what differs in a schema between two connections, such as production and staging.
- **Frugal with tokens.** The eight tool definitions take 5,484 bytes of `tools/list`, and a session that loads them and asks ten typical questions an estimated 2,550 tokens. Output is compact text, TSV by default, and plans, column statistics and server health come back as a few summary lines, not raw dumps. A result past 10 KB is written to a private CSV file and summarized, so the agent reads or greps the file instead of the rows: `SELECT * FROM events ORDER BY id` on a 20,000-row table comes back in 963 bytes, with its first 1,000 rows (`max_rows`) in the file. The [token benchmark](docs/benchmark.md) measures this, and fails CI when a response grows more than 10%.

## Quickstart

On macOS or Linux:

1. Install the latest release in `~/.local/bin`, once its SHA-256 matches the release's checksums:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/Jpgomesf/pgglass/main/install.sh | sh
   ```

2. Create `~/.config/pgglass/config.json`, with a role that can only read:

   ```json
   {
     "connections": {
       "my-db": {
         "source": "direct",
         "dsn": "postgres://readonly_user:password@localhost:5432/mydb"
       }
     }
   }
   ```

3. Add it to Claude Code:

   ```bash
   claude mcp add --transport stdio --scope user pgglass -- ~/.local/bin/pgglass
   ```

4. Check the config and the connection's role:

   ```bash
   ~/.local/bin/pgglass doctor
   ```

   It prints a line per connection, such as `ok my-db: role readonly_user: no findings`, and exits 1 if one fails.

Then ask your agent about the database, such as "which tables in my-db are the largest?". [Install](#install) has the release archive, Docker and source builds; [Configure](#configure) has the other credential sources.

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

Tables are also MCP resources, `pgglass://<connection>/<schema>/<table>`, read through the same role check, limits and audit log as the tools: in Claude Code, `@`-mention one to add it to a prompt as `pg_schema` describes it. The server lists the tables of the connections the agent has used, so listing them opens no other connection, and the listing keeps to 10 KB. See [resources](docs/usage.md#mention-tables-as-resources).

## Many databases, one call

<picture><source media="(prefers-color-scheme: dark)" srcset="docs/assets/diagrams/fan-out-dark.png"><img alt="One pg_query call runs the same statement on several connections in parallel; their rows come back merged under a connection column, with a line for each connection that failed." src="docs/assets/diagrams/fan-out.png" width="830"></picture>

In place of `connection`, `pg_query` takes `connections`, up to 10 names, and runs the statement on each in parallel:

```json
{"connections": ["staging-app", "prod-app"], "sql": "SELECT count(*) FROM orders"}
```

```
connection	count
staging-app	1204
prod-app	98311
(2 rows)
```

- The statement is checked once. Each connection then gets what a call on it alone would: the role check, its `search_path`, the limits and one audit line.
- A connection that fails adds a line with its error after the rows; the call fails only when every connection does.
- To compare structure rather than rows, `pg_diff_schema` lists what differs in one schema between two connections.

See [Fan-out](docs/usage.md#fan-out) and [Compare a schema across connections](docs/usage.md#compare-a-schema-across-connections).

## How it compares

Other Postgres MCP servers, as their own READMEs and docs describe them at the version named, read on 2026-10-14. The `tools/list` sizes were measured on 2026-10-13, each server in its published version against the same fixture ([method](docs/benchmark.md#other-servers)).

| | pgglass 1.0.0 | [Postgres MCP Pro](https://github.com/crystaldba/postgres-mcp/blob/v0.3.0/README.md) 0.3.0 | [DBHub](https://github.com/bytebase/dbhub/blob/v1.5.0/README.md) 1.5.0 | [MCP Toolbox](https://github.com/googleapis/mcp-toolbox/blob/v1.14.0/README.md) 1.14.0 | [Reference server](https://www.npmjs.com/package/@modelcontextprotocol/server-postgres/v/0.6.2) 0.6.2, deprecated |
|---|---|---|---|---|---|
| Databases | PostgreSQL | PostgreSQL | PostgreSQL, MySQL, MariaDB, SQL Server, Oracle, SQLite | PostgreSQL, AlloyDB, BigQuery, MySQL and many more | PostgreSQL |
| Read-only | Always: each statement parsed with the Postgres parser and run in a `READ ONLY` transaction that is rolled back; the role audited on first use | With `--access-mode=restricted`: read-only transactions under a time limit, and SQL parsed with pglast so that only read-only statements pass | With [`readonly = true`](https://dbhub.ai/tools/execute-sql) on a tool ([default `false`](https://dbhub.ai/config/toml)): a keyword allow-list, then a `READ ONLY` transaction | [Read-only mode](https://mcp-toolbox.dev/documentation/configuration/security/read-only/) on Cloud SQL, AlloyDB and BigQuery sources; the `postgres` source has no such setting | Every query in a `READ ONLY` transaction that is rolled back |
| Databases per server | Several; one `pg_query` call runs on up to 10 | One, `DATABASE_URI` | Several, as `[[sources]]` in `dbhub.toml` | Several, as `sources` in `tools.yaml` | One, given as its argument |
| Tools, `tools/list` bytes | 8, 5,484 | 9, 5,289 | 2, 1,479 | 29, 20,693 (prebuilt `postgres`) | 1, 131 |

Each of them does more than this table shows, such as Postgres MCP Pro's index tuning, DBHub's web workbench and MCP Toolbox's custom tools: follow the links. The [token benchmark](docs/benchmark.md) also measures the size of pgglass's responses, which `tools/list` says nothing of.

## Safety model

The calling agent is treated as untrusted: instructions can reach it through the data it reads (prompt injection). These layers hold whatever SQL it sends. [The safety model](docs/safety-model.md) has each in full.

1. [**The database role is the real boundary.**](docs/safety-model.md#role-boundary) The server never gives the agent more than the role it connects as: connect as a role that can read only what the agent should see.
2. [**The role is checked.**](docs/safety-model.md#role-check) On a connection's first use, the server audits what its role can do beyond reading, and `role_check: strict` refuses one with a critical finding.
3. [**SQL is parsed, not pattern-matched.**](docs/safety-model.md#sql-parsing) `pg_query` accepts one `SELECT`, `EXPLAIN` of one or `SHOW`, checked with the Postgres parser, and rejects a denylist of functions and catalogs.
4. [**Every statement runs read-only and is rolled back.**](docs/safety-model.md#read-only-transaction) Each runs alone in a `READ ONLY` transaction that is always rolled back, with `statement_timeout` and `lock_timeout` set.
5. [**Results are bounded.**](docs/safety-model.md#bounded-results) Rows past `max_rows` or `max_bytes` are not kept, and a result past 10 KB is written to a private CSV file and summarized.
6. [**Calls are audited.**](docs/safety-model.md#audit-log) Each call writes one JSON line to stderr; SQL text, parameters and values are never logged.
7. [**HTTP stays on this machine unless told otherwise.**](docs/safety-model.md#http) `--http` listens on loopback by default, any other address needs a bearer token, and requests from web pages on other origins are refused.
8. [**A connection can ask first.**](docs/safety-model.md#confirmation) With `confirm` set, the user sees the statement and its `params` through the client, and it runs only if they accept.

[What these layers do not cover](docs/safety-model.md#what-these-layers-do-not-cover), in short:

- The parser knows names, not what is behind them, and the denylist holds only known functions: a function the role may execute can call `dblink` or another escape internally, and a view over a denied catalog reads it.
- Whatever the role can `SELECT`, the agent can read and repeat, including what Postgres shows the role about the server, such as settings and other sessions' SQL.
- A superuser connection has no role boundary: use `role_check: strict`, or a role that is not a superuser.
- A query may use the database's CPU and I/O, and hold locks that delay DDL, until `statement_timeout` ends it.
- `--http` serves plain HTTP: to reach it from elsewhere, put a proxy that terminates TLS in front of it, and set the token.

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

<details>
<summary>MCP clients, credentials and spill files in the container</summary>

- Give your MCP client that command, as in `claude mcp add pgglass -- docker run --rm -i ...`, and pin a version with a tag such as `:1.0.0`.
- `--user` runs the server as you, so it can read a `config.json` only you can read.
- Credentials resolve inside the container: pass the variables that `env` sources read with `-e`, and keep `.env` files in the mounted directory.
- A result past 10 KB is written to a file inside the container, out of the agent's reach. To share these files, run `mkdir -p ~/.cache/pgglass` and add `-v "$HOME/.cache/pgglass:$HOME/.cache/pgglass" -e XDG_CACHE_HOME="$HOME/.cache"`: the directory has the same path on both sides, so the agent can open the path the server reports.

</details>

<details>
<summary>Serve Streamable HTTP from the container</summary>

```bash
export PGGLASS_HTTP_TOKEN="$(openssl rand -hex 32)"
docker run --rm -p 127.0.0.1:8411:8411 -e PGGLASS_HTTP_TOKEN --user "$(id -u):$(id -g)" \
  -v ~/.config/pgglass:/etc/pgglass:ro \
  ghcr.io/jpgomesf/pgglass --config /etc/pgglass/config.json --http 0.0.0.0:8411
```

In the container the server has to listen on every interface, which requires the token; publishing the port on `127.0.0.1` keeps it on this machine. Clients connect to `http://127.0.0.1:8411/mcp` with the header `Authorization: Bearer $PGGLASS_HTTP_TOKEN`.

</details>

### From source

```bash
go install github.com/jpgomesf/pgglass@latest
```

Or clone the repository and run `make build`.

## Configure

Create `~/.config/pgglass/config.json` (`$XDG_CONFIG_HOME/pgglass/config.json` when `XDG_CONFIG_HOME` is set to an absolute path), or pass another file with `--config`:

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

Upgrading from mcp-postgres? [Upgrading](docs/upgrading.md) lists what to move and rename.

## Documentation

| Doc | Description |
|-----|-------------|
| [Usage Guide](docs/usage.md) | Tools, arguments, formats, large results, errors |
| [Configuration](docs/configuration.md) | Credential sources, connection options, defaults, PgBouncer, command line, doctor, Streamable HTTP |
| [Safety model](docs/safety-model.md) | The safety model in full: each layer, and what the layers do not cover |
| [How it's built](docs/architecture.md) | The packages, the path of one tool call through them, and how pgglass is tested |
| [Token benchmark](docs/benchmark.md) | What pgglass puts in an agent's context, measured, and how other servers' tool lists compare |
| [Extending](docs/extending.md) | How to add custom credential sources (Vault, GCP, etc.) |
| [Upgrading](docs/upgrading.md) | Moving to pgglass from its earlier name: config location, client entry, permission rules, spill files |
| [Contributing](CONTRIBUTING.md) | Branch strategy, commit conventions, development workflow |
| [Releasing](docs/releasing.md) | What a release publishes, cutting one, the MCP Registry, platforms |

## License

[MIT](LICENSE)
