# Usage Guide

Once the server is connected to your agent (for example Claude Code), you ask in natural language and the agent calls the tools. Every tool is read-only, and calls can run in parallel, on one connection or across several.

## Tools

| Tool | Arguments | Returns |
|------|-----------|---------|
| `pg_list_connections` | none | Each connection, in name order, with its role-check status |
| `pg_schema` | `connection`, `schema`?, `table`? | Schemas, the relations of one schema, or one relation in detail |
| `pg_search` | `connection`, `pattern`, `limit`? | Relations, columns and functions whose name contains `pattern` |
| `pg_query` | `connection`, `sql`, `params`?, `schema`?, `format`?, `max_rows`? | The rows of one read-only statement |

## Discover connections

```
Connections (2):
  prod-app [aws_secrets_manager, vpn, schema:app, loaded]
    role app_reader: no findings
  staging-app [env, schema:app]
    role not checked yet: checked on first use
```

A connection's role is audited on its first use (see [role check](configuration.md#role-check)); from then on `pg_list_connections` shows the report.

## Explore the schema

`pg_schema` goes one level deeper with each argument, so the agent reads only what it needs:

- no `schema`: one line per schema, `app: 7 relations -- Fixture application schema`
- `schema`: one line per table, view, materialized view or foreign table, `app.orders: table, ~50 rows, 48 kB`, with `RLS` when row-level security is on
- `schema` and `table`: the relation in a compact DDL-like form, with column types, defaults, identity and generated columns, enum labels, comments, constraints, indexes, policies, triggers and, for views, the definition

`table` without `schema` looks in the connection's `default_schema`. Names are matched exactly, unquoted, as the catalog stores them. Results are cached for 5 minutes. Output longer than 10 KB is cut after the last whole line that fits, followed by `… 312 of 4000 lines shown (output limit 10000 bytes): use pg_search to find names`.

`pg_search` finds names that contain `pattern`, ignoring case; `%` and `_` are plain characters. Relations come first, then columns, then functions, `limit` (default 50) at most:

```
app.customer_order_totals: view
app.orders: table
app.customer_order_totals.order_count: bigint
```

Only what the connection's role can read shows up.

## Query

`pg_query` runs one `SELECT`, `EXPLAIN [ANALYZE] SELECT` or `SHOW`. Anything else is rejected before it reaches the database (see the [safety model](../README.md#safety-model)).

| Argument | Description |
|----------|-------------|
| `connection` | Connection name |
| `sql` | One statement; `$1`, `$2`, ... take `params` |
| `params` | Values for `$1`, `$2`, ...: strings, numbers (sent exactly as written), booleans or `null`. Pass arrays and objects as strings and cast them, e.g. `'{1,2}'` with `$1::int[]` |
| `schema` | `search_path` for this query only; default: the connection's `default_schema`. `net`, `aws_s3` and `aws_lambda`, whose functions the guard denies, are refused |
| `format` | `tsv`, `json` or `markdown`; default: `format` in config (`tsv`) |
| `max_rows` | Row cap for this query; it can only lower the configured `max_rows` |

Values come back exactly as Postgres prints them, so `uuid`, `numeric`, `jsonb`, arrays and timestamps keep their text form, and repeated column names are kept:

```
id	status	total	placed_at
1	paid	12.34	2024-03-01 15:00:00+00
2	shipped	24.68	2024-03-01 21:00:00+00
(2 rows)
```

The last line counts the rows, and says when a limit cut the result: `(1000 rows; truncated: max_rows=1000)`.

### Formats

| Format | Layout | NULL |
|--------|--------|------|
| `tsv` | Header and rows, tab-separated; tab, newline, carriage return and backslash written `\t`, `\n`, `\r`, `\\` | `\N` |
| `json` | `{"columns":[{"name","type"}],"rows":[[...]]}` on one line; numbers, booleans, `json` and `jsonb` as JSON, everything else as strings | `null` |
| `markdown` | A table; backslash, pipe, newline and carriage return escaped | `NULL` (a value that is the text `NULL` is written `\NULL`) |

Inline, cells longer than 200 characters are cut, with a note of how much was dropped: `…[+1234 chars]`.

### Large results

When the output would exceed 10 KB, the full result is written as CSV to a new file and the tool returns a summary instead:

```
Result too large to return inline (33700 bytes, limit 10000): 1000 rows × 3 columns, truncated=true (max_rows=1000).
Full result: /home/me/.cache/mcp-postgres/spill-pg_query-prod-app-20261012T180244Z-2684963475.csv
It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.
Use Read or Grep on the file instead of re-running the query.

Columns:
column	type
id	int8
kind	text
occurred_at	timestamptz

First 5 rows:
...
```

The summary lists at most 20 columns, previews only as many of the first 5 rows as fit in 10 KB, and cuts preview cells at 64 characters.

Spill files live in `mcp-postgres` under your user cache directory (`~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux), readable only by you (mode 0600). Spill files older than `spill_ttl_hours` are removed at startup, then at most once an hour as `pg_query` returns results, whichever server instance wrote them.

## Errors

A failed call returns a result with `isError` set and the reason as text:

- `query rejected: DeleteStmt is not allowed: only SELECT, EXPLAIN SELECT and SHOW are` when the statement is not read-only
- `query rejected: schema net is not allowed as the search_path: its functions are denied` when `schema`, or `default_schema` without it, names a schema whose functions the guard denies
- Postgres errors as psql prints them, with SQLSTATE and position, then detail and hint:

  ```
  ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8
  HINT: Perhaps you meant to reference the column "customers.name".
  ```

- `connection "prod" refused (role_check strict): its role can do more than read.`, followed by the findings
- invalid arguments, such as an unknown connection or argument name

## Audit log

Each tool call writes one JSON line to stderr:

```json
{"time":"2026-10-12T14:59:12.2-03:00","level":"INFO","msg":"tool call","tool":"pg_query","connection":"prod-app","sql_sha256":"46880a16e933","sql_len":30,"duration_ms":41,"rows":1,"truncated":false,"outcome":"ok","sqlstate":""}
```

`sql_sha256` is the first 12 hex digits of the statement's SHA-256, so repeated statements can be matched without logging them. `outcome` is `ok`, `guard_reject`, `db_error` (with `sqlstate` when Postgres raised it), `preflight_block` or `error` (for example, invalid arguments). SQL text, parameters and values are never logged.

## Parallel calls

Every connection has its own pool, so calls do not wait on each other, on one connection or across several:

> "Compare the row count of the orders table between staging and production"

Two `pg_query` calls run at the same time, one per connection.

## Tips

- **Start with `pg_schema` or `pg_search`.** Agents write better SQL when they know the exact names and types.
- **Set `default_schema`** so unqualified names resolve without a `schema` argument.
- **Aggregate in SQL.** A `count(*)` or `GROUP BY` answers in one row where reading rows spills a file.

## Profile a table

`pg_profile` (`connection`, `table`, `schema`?, `column`?) shows what the columns of a table, partitioned table or materialized view hold, one line per column: the first 50, with a note counting the rest, or only `column`. `table` is `schema.table`, or a name in `schema` or else the connection's `default_schema`, matched exactly.

The figures come from `pg_stats`, which reads nothing from the table: the share of nulls, distinct values (a count, or a share of the rows when `pg_stats` expects them to grow with the table), average width in bytes, the 5 most common values with their share of the rows, and the first and last histogram bounds, an estimate of the range without the most common values:

```
app.orders: ~50 rows, from pg_stats
column	type	nulls	distinct	width	common	min	max
id	bigint	0%	100%	8		1	50
status	app.order_status	0%	4	4	'paid' 26%, 'shipped' 26%, 'pending' 24%, 'cancelled' 24%		
total	numeric(12,2)	0%	100%	6		12.34	617.00
...
(9 rows)
```

When `pg_stats` has nothing on the columns, because the table was never analyzed or row-level security hides its statistics from the role, `pg_profile` samples the table instead, at most 10,000 rows taken with `TABLESAMPLE SYSTEM`, and counts each column's nulls and distinct values in the sample:

```
public.fresh: sampled 300 rows with TABLESAMPLE SYSTEM (100) LIMIT 10000, since pg_stats has nothing on these columns (never analyzed, or hidden by row-level security)
column	type	nulls	distinct
id	integer	0	300
note	text	100	7
(2 rows)
```

Both read as the connection's role: `pg_stats` shows only the columns the role can `SELECT`, and `pg_profile` says so when that leaves none; the sample is subject to privileges and row-level security. Relations the guard denies, such as `pg_authid`, are refused, since their statistics hold their values. Output over 10 KB is written to a file, as `pg_query`'s is.
