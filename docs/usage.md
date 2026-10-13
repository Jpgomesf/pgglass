# Usage Guide

Once the server is connected to your agent (for example Claude Code), you ask in natural language and the agent calls the tools. Every tool is read-only, and calls can run in parallel, on one connection or across several.

## Tools

| Tool | Arguments | Returns |
|------|-----------|---------|
| `pg_list_connections` | none | Each connection, in name order, with its role-check status |
| `pg_schema` | `connection`, `schema`?, `table`? | Schemas, the relations of one schema, or one relation in detail |
| `pg_search` | `connection`, `pattern`, `limit`? | Relations, columns and functions whose name contains `pattern` |
| `pg_query` | `connection` or `connections`, `sql`, `params`?, `schema`?, `format`?, `max_rows`? | The rows of one read-only statement, from one connection or up to 10 |
| `pg_explain` | `connection`, `sql`, `analyze`?, `schema`? | A few lines on the plan of one `SELECT`: cost, the costliest nodes, flags |
| `pg_profile` | `connection`, `table`, `schema`?, `column`? | One line per column: nulls, distinct values, most common values, range |
| `pg_diff_schema` | `left`, `right`, `schema`? | What differs in a schema between two connections |
| `pg_health` | `connection` | One line per finding on connections, sessions, cache, dead rows, indexes, wraparound and replication |

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

`table` without `schema` looks in the connection's `default_schema`. Names are matched exactly, unquoted, as the catalog stores them. Results are cached for 5 minutes. Output longer than 10 KB is cut after the last whole line that fits, followed by `… 312 of 4000 lines shown (output limit 10000 bytes): use pg_search to find names`. A listing reads only its first 500 schemas or relations from the catalog, however many there are; the note counts them all.

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
| `connections` | In place of `connection`: up to 10 connection names to run the statement on in parallel (see [Fan-out](#fan-out)) |
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

Spill files live in `mcp-postgres` under your user cache directory (`~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux), readable only by you (mode 0600). Without one (`HOME` unset), they live in `mcp-postgres-<uid>` in the temp directory, which the server creates: it must be a directory, not a symlink, that you own with mode 0700, or the server does not start. Spill files older than `spill_ttl_hours` are removed at startup, then at most once an hour as tools return results, whichever server instance wrote them.

### Fan-out

In place of `connection`, `pg_query` takes `connections`, up to 10 names, and runs the statement on each in parallel:

> "Count the orders in staging and production"

```
connection	count
staging-app	1204
prod-app	98311
(2 rows)
```

- The statement is checked once. Each connection then gets what a call on it alone would: the role check, its `search_path` (`schema`, or its own `default_schema`), the limits and one audit line.
- Rows come in the order of `connections`, after a first column, `connection`. The footer names each connection a limit cut, `(1500 rows; truncated: prod-app max_rows=1000)`, and a result over 10 KB spills to one CSV file.
- When the connections return different columns, by name or type, each gets its own section under its name, and the sections share the 10 KB.
- A connection that fails adds a line after the rows, with its error as a call on it alone would show it, such as `prod-app: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)`. The call fails only when every connection does.

## Explain a query

`pg_explain` takes `connection`, `sql`, `analyze`? and `schema`?, and summarizes the plan of one `SELECT` instead of returning EXPLAIN's output. `sql` passes the same checks as in `pg_query`, and only a `SELECT` can be explained. `schema` works as in `pg_query`. Without `analyze` the query is only planned. With `analyze: true` it runs in the same read-only transaction, which is rolled back, and under the same `statement_timeout`, to report actual times, rows and buffers:

```
total cost 678.64, 5.496 ms, 5000 rows, 333 buffers hit, 0 read
top nodes by own time:
  Sort: 3.195 ms, 5000 rows
  Seq Scan on app.events: 2.087 ms, 5000 rows
flags:
  Sort: 5000 rows, planned 25
  Sort: spilled to disk (external merge, 504 kB)
  Seq Scan on app.events: 5000 rows, planned 25
  Seq Scan on app.events: full scan of ~20000 rows
```

The 5 top nodes are ranked by what each takes on its own: its total less its children's. That is cost without `analyze`, and time with it, summed over loops and parallel workers. Rows are per loop, as in EXPLAIN. The flags are:

- a seq scan of a relation with over 10,000 rows (`pg_class.reltuples`)
- with `analyze`: a node that returned more than 10 times its estimated rows, or less than a tenth of them
- with `analyze`: a sort, hash or hash aggregate that spilled to disk
- a nested loop whose outer side returns over 10,000 rows, so that its inner side runs that many times

Error positions point into `sql` as written. A summary over 10 KB is written to a spill file, one line per row, like a large `pg_query` result.

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

## Compare a schema across connections

`pg_diff_schema` compares one schema on two connections, typically production and staging. It takes `left` and `right`, two connection names, and `schema` (default `public`), and reads each side with one catalog query:

```
schema app, prod → staging: 4 differences (- only in prod, + only in staging)
+ table coupons
~ column orders.total: numeric(12,2) NOT NULL → numeric(14,2) NOT NULL
+ index orders.orders_placed_at_idx
- table tenant_notes
```

`~` marks what both define differently, left then right. It compares relations (their kind and whether row-level security is on), columns (type, `NOT NULL`, default, identity or generation), constraints, indexes and RLS policies; an index that implements a constraint shows as the constraint. Relations come in name order, each followed by its columns, constraints, indexes and policies, and a relation only one side has takes one line. When nothing differs, the result says `identical` and how many relations it compared.

- Only what each connection's role can read is compared, as in `pg_schema`, and partitions are left out. A schema that one side lacks, or whose role lacks `USAGE` on it, is an error naming that side.
- Definitions are compared as each server prints them, so servers of different major versions can differ in spelling alone.
- Output past 10 KB is written to a CSV file, one difference per row, and summarized under the first line.
- Both connections go through the role check, and the audit line names both: `"connection":"prod,staging"`.

## Check server health

`pg_health` (`connection`) reads the statistics views and catalogs and returns one row per finding, in the configured format, spilled to a file like a query result when it is too large:

```
section	detail
connections	7 of max_connections 100: active 1, idle in transaction 1; the state of 5 is not visible to role app_reader
long_running	none
long_running	other roles' sessions are not visible to role app_reader
blocked	pid 79, database app, state not visible to role app_reader, blocked by 78
cache_hit	99.99% in database app
dead_tuples	app.orders: 25.0% dead (25 dead, 75 live), never autovacuumed
unused_indexes	app.orders_customer_id_idx on app.orders: 16 kB
xid_wraparound	oldest age(datfrozenxid) 17 (database app): 0.0% of autovacuum_freeze_max_age 200000000
replication	not a replica
(9 rows)
```

| Section | Shows |
|---------|-------|
| `connections` | Backends connected to a database, against `max_connections`, by state |
| `long_running` | Client sessions, other than idle ones, whose query started over a minute ago: pid, database, state, duration, wait event, application name |
| `blocked` | Sessions that `pg_blocking_pids` reports waiting for a lock, and the pids blocking them |
| `cache_hit` | The share of this database's block reads found in shared buffers |
| `dead_tuples` | The 5 tables of this database with the highest share of dead rows, and their last autovacuum |
| `unused_indexes` | This database's indexes not scanned since the statistics were reset, largest first; unique indexes, primary keys and exclusion constraints' indexes are left out |
| `xid_wraparound` | The oldest `age(datfrozenxid)` of any database, against `autovacuum_freeze_max_age` |
| `replication` | On a replica, how long ago it replayed its last transaction, and how many received WAL bytes it has yet to replay |

`long_running`, `blocked` and `unused_indexes` stop at 10 rows and say how many more there are. Sessions are those of the whole server, and their query text is never shown: it can hold secrets. A role without the privileges of `pg_read_all_stats` sees the state, timing and wait event of its own sessions only, as above. A section whose view or function the role may not use says `not visible to role <name>`, and the other sections still run.

## Errors

A failed call returns a result with `isError` set and the reason as text:

- `query rejected: DeleteStmt is not allowed: only SELECT, EXPLAIN SELECT and SHOW are` when the statement is not read-only
- `query rejected: only a SELECT can be explained, not EXPLAIN or SHOW` when `pg_explain` gets another statement the guard allows
- `query rejected: schema net is not allowed as the search_path: its functions are denied` when `schema`, or `default_schema` without it, names a schema whose functions the guard denies
- Postgres errors as psql prints them, with SQLSTATE and position, then detail and hint:

  ```
  ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8
  HINT: Perhaps you meant to reference the column "customers.name".
  ```

- `connection "prod" refused (role_check strict): its role can do more than read.`, followed by the findings
- invalid arguments, such as an unknown connection or argument name

## Audit log

Each tool call writes one JSON line to stderr, or one per connection for a [fan-out](#fan-out):

```json
{"time":"2026-10-12T14:59:12.2-03:00","level":"INFO","msg":"tool call","tool":"pg_query","connection":"prod-app","sql_sha256":"46880a16e933","sql_len":30,"duration_ms":41,"rows":1,"truncated":false,"outcome":"ok","sqlstate":""}
```

`connection` names both connections of a `pg_diff_schema` call, `prod,staging`. `sql_sha256` is the first 12 hex digits of the statement's SHA-256, so repeated statements can be matched without logging them; it is empty, and `sql_len` 0, for tools that take no SQL. `outcome` is `ok`, `guard_reject`, `db_error` (with `sqlstate` when Postgres raised it), `preflight_block` or `error` (for example, invalid arguments). SQL text, parameters and values are never logged.

## Parallel calls

Every connection has its own pool, so calls do not wait on each other, on one connection or across several:

> "Compare the row count of the orders table between staging and production"

The agent can make two `pg_query` calls, which run at the same time, or one with `connections` (see [Fan-out](#fan-out)).

## Tips

- **Start with `pg_schema` or `pg_search`.** Agents write better SQL when they know the exact names and types.
- **Set `default_schema`** so unqualified names resolve without a `schema` argument.
- **Aggregate in SQL.** A `count(*)` or `GROUP BY` answers in one row where reading rows spills a file.
- **Look before you scan.** `pg_explain` plans a query without running it, and `pg_profile` reads what a table's columns hold from `pg_stats`.
