# Token benchmark

pgglass is meant to be light on an agent's context. This benchmark measures what it puts there: the tool definitions, the instructions, and the response to each of ten questions an agent asks. A check in CI fails when one of them grows more than 10%.

## Method

`TestTokenBudget` in `internal/bench` connects an MCP client to pgglass through the SDK's in-memory transport and records, in bytes:

- `tools/list`: the tool definitions, as compact JSON
- `instructions`: the instructions the server sends on `initialize`
- each question: the text of the tool's result, which is what the agent reads

Tokens are estimated as bytes divided by 4, rounded up: an estimate, not a tokenizer's count.

The server runs with the default settings (`max_rows` 1000, results over 10 KB written to a spill file) and two connections, both as `ro_clean`, a role that can only read, with `default_schema` `app`:

- `prod-app`: the `app` fixture of `internal/testdb`, with 50 customers, 50 orders and 20,000 events
- `staging-app`: a copy of it after a migration that adds a table, a column and an index, and drops a `NOT NULL`

The sizes were the same on every run, and on Postgres 15, 16, 17 and 18, since:

- a spill file's path counts as `/home/me/.cache/pgglass/spill-…-4294967295.csv`, under a typical cache directory and with the longest random part, not as the test's temporary directory
- autovacuum is off on the benchmark's server, so no autovacuum worker shows in `pg_health`'s count of connections
- `pg_explain` plans without `analyze`, whose timings vary

## Scenarios

The questions run in this order, as an agent would ask them:

| Scenario | The agent asks | Call |
|---|---|---|
| list connections | Which databases can I query? | `pg_list_connections` |
| find a table | Where are orders kept? | `pg_search`, `pattern` `order` |
| describe a table | What does an order hold? | `pg_schema`, `table` `orders` |
| filtered query | What are the last 10 paid orders? | `pg_query`: `SELECT id, status, total, placed_at FROM orders WHERE status = $1 ORDER BY placed_at DESC LIMIT 10`, `$1` = `paid` |
| large query, spilled | Show me the events | `pg_query`: `SELECT * FROM events ORDER BY id`, cut at 1,000 rows and spilled |
| profile a column | Which statuses do orders have? | `pg_profile`, `table` `orders`, `column` `status` |
| explain a query | How costly is counting recent events by kind? | `pg_explain`: `SELECT kind, count(*) FROM events WHERE occurred_at >= '2024-01-10' GROUP BY kind` |
| server health | Is the server healthy? | `pg_health` |
| diff two databases | How does staging differ from production? | `pg_diff_schema`, `prod-app` against `staging-app`, `schema` `app` |
| fan-out | How many orders, and how much revenue, in each? | `pg_query` on both connections: `SELECT count(*) AS orders, sum(total) AS revenue FROM orders` |

## Numbers

The baseline, `internal/bench/testdata/bench-baseline.json`, holds these sizes:

| Scenario | Bytes | Tokens (est.) |
|---|---:|---:|
| tools/list | 5484 | 1371 |
| instructions | 1064 | 266 |
| list connections | 177 | 45 |
| find a table | 95 | 24 |
| describe a table | 785 | 197 |
| filtered query | 415 | 104 |
| large query, spilled | 963 | 241 |
| profile a column | 182 | 46 |
| explain a query | 187 | 47 |
| server health | 549 | 138 |
| diff two databases | 220 | 55 |
| fan-out | 79 | 20 |
| total | 10200 | 2550 |

`total` is a session that loads the tools and asks each question once.

## The check

`make bench` runs `internal/bench`. `TestTokenBudget` fails when a scenario takes more than 10% more bytes than in the baseline, or `tools/list` more than 6,000 bytes, and `TestDocsShowTheBaseline` when the table above differs from the baseline. CI runs it in a job of its own, and with every other test on each supported Postgres.

When a change makes a response larger on purpose, or smaller, `make bench-baseline` measures again and rewrites the baseline and the table above: commit both.

## Other servers

The `tools/list` of other Postgres MCP servers, measured on 2026-10-13. Each ran in a container, in its published version, against a disposable Postgres loaded with the same fixture, connected as `ro_clean`. A JSON-RPC client sent `initialize` (revision 2025-06-18), `notifications/initialized` and `tools/list` over stdio, and counted the bytes of the tools array as the server sent it, whitespace removed. Measured the same way that day, pgglass's took 5,484 bytes, as in the benchmark, and its instructions 1,064; none of the others sends instructions.

| Server | Version | Run with | Tools | `tools/list` bytes | Tokens (est.) |
|---|---|---|---:|---:|---:|
| [Reference server](https://www.npmjs.com/package/@modelcontextprotocol/server-postgres) (deprecated) | 0.6.2 | `npx -y @modelcontextprotocol/server-postgres@0.6.2 <dsn>` | 1 | 131 | 33 |
| [DBHub](https://www.npmjs.com/package/@bytebase/dbhub) | 1.5.0 | `npx -y @bytebase/dbhub@1.5.0 --transport stdio --dsn <dsn>` | 2 | 1479 | 370 |
| [Postgres MCP Pro](https://github.com/crystaldba/postgres-mcp) | 0.3.0 | `docker run -i --rm -e DATABASE_URI=<dsn> crystaldba/postgres-mcp --access-mode=restricted` | 9 | 5289 | 1323 |
| [MCP Toolbox for Databases](https://www.npmjs.com/package/@toolbox-sdk/server), prebuilt `postgres` | 1.14.0 | `npx -y @toolbox-sdk/server@1.14.0 --prebuilt postgres --stdio`, with `POSTGRES_HOST` and the like | 29 | 20693 | 5174 |
| [postgres-mcp-server](https://www.npmjs.com/package/@henkey/postgres-mcp-server) | 1.0.7 | `npx -y @henkey/postgres-mcp-server@1.0.7 --connection-string <dsn>` | 18 | 27749 | 6938 |

Postgres MCP Pro ran from its published image, which holds 0.3.0: `uvx postgres-mcp@0.3.0` fails to start today, since it installs version 2 of the Python MCP SDK, where `FastMCP` was renamed. MCP Toolbox ran on linux/amd64: its npm package has no linux/arm64 binary.

These figures count tool definitions only: they say nothing of the size of the responses, or of what each tool can do.
