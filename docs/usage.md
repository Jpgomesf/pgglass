# Usage Guide

Once the server is connected to your agent (for example Claude Code), you ask in natural language and the agent calls the tools. Every tool is read-only, and calls can run in parallel, on one connection or across several.

On connecting, the server tells the agent how to use the tools well: start with `pg_list_connections`, look names up with `pg_schema` or `pg_search` instead of guessing them, pass values as `params`, run one statement on several connections with `connections`, and read a spill file instead of running the query again.

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

Postgres keeps names of at most 63 bytes and cuts a longer one to its first 63, which may name another object. So a `schema`, `table` or `column` over 63 bytes is refused before any query: `table is 64 bytes: Postgres names take at most 63, and it cuts a longer one`. Connection names and `default_schema` take at most 63 bytes too, or the server does not start (see [Configuration](configuration.md#validation-rules)).

## Discover connections

```
Connections (2):
  prod-app [aws_secrets_manager, vpn, schema:app, confirm, loaded]
    role app_reader: no findings
  staging-app [env, schema:app]
    role not checked yet: checked on first use
```

A connection's role is audited on its first use (see [role check](configuration.md#role-check)); from then on `pg_list_connections` shows the report. Past 10 KB, the list is written to a spill file, one line per row, like a large `pg_query` result. `confirm` marks a connection that asks the user before a statement runs on it (see [Confirmation](#confirmation)).

The findings also start the first response on the connection, which keeps to 10 KB with them: they take at most half of it, as many lines as fit and then a line that points to `pg_list_connections`, and the rest of the response fits what they leave.

## Explore the schema

`pg_schema` goes one level deeper with each argument, so the agent reads only what it needs:

- no `schema`: one line per schema, `app: 7 relations -- Fixture application schema`
- `schema`: one line per table, view, materialized view or foreign table, `app.orders: table, ~50 rows, 48 kB`, with `RLS` when row-level security is on
- `schema` and `table`: the relation in a compact DDL-like form, with column types, defaults, identity and generated columns, enum labels, comments, constraints, indexes, policies, triggers and, for views, the definition. A column is `NOT NULL` only when Postgres has checked every row, so not under a `NOT VALID` not-null constraint (PostgreSQL 18), whose older rows may hold nulls; `pg_diff_schema` compares columns the same way

`table` without `schema` looks in the connection's `default_schema`. Names are matched exactly, unquoted, as the catalog stores them. Results are cached for 5 minutes. Output longer than 10 KB is cut after the last whole line that fits, followed by `… 312 of 4000 lines shown (output limit 10000 bytes): use pg_search to find names`. A listing reads only its first 500 schemas or relations from the catalog, however many there are; the note counts them all. A comment longer than 200 characters is cut to its first 200, with a note of how many it drops, `...[+1234 chars]`: so cut, the comments of 500 relations, or of a relation's hundreds of columns, keep well within the default `max_bytes`.

`pg_search` finds names that contain `pattern`, ignoring case; `%` and `_` are plain characters. Relations come first, then columns, then functions, `limit` (default 50) at most:

```
app.customer_order_totals: view
app.orders: table
app.customer_order_totals.order_count: bigint
```

When there are more matches, a last line names the limit that cut them: `(truncated: limit=50)`, or `max_rows` when it caps `limit`. Only what the connection's role can read shows up. Matches over 10 KB are written to a spill file, one per row, like a large `pg_query` result, and the summary names that limit instead: `truncated=true (limit=500)`.

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

A value or Postgres error can hold bytes that are not UTF-8: a session in a `SQL_ASCII` database can write any byte to the shared catalogs, such as a database comment, and `pg_stat_activity` shows other databases' queries in their own encoding. Each run of such bytes comes back as one U+FFFD, `�`, in the spill file too, so that 10 KB holds as the client decodes a response.

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
Full result: /home/me/.cache/pgglass/spill-pg_query-prod-app-20261012T180244Z-2684963475.csv
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

After the file, the summary lists the first 20 columns and previews the first 5 rows, as many of each as fit in 10 KB, and cuts preview cells at 64 characters.

Spill files live in `pgglass` under your user cache directory (`~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux), readable only by you (mode 0600). Without one (`HOME` unset), they live in `pgglass-<uid>` in the temp directory, which the server creates: it must be a directory, not a symlink, that you own with mode 0700, or the server does not start. Spill files older than `spill_ttl_hours` are removed at startup, then at most once an hour as tools return results, whichever server instance wrote them.

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
- When the connections return different columns, by name or type, each gets its own section under its name, and the sections, names included, share the 10 KB: a section past its share spills to a CSV file of its own. When its share is too small for the summary, the section only names the file and counts the rows, `Full result: <path>` then `(100 rows)`. Sections that still take more than their room, as long connection names and a long spill directory path can make them, each spill to a CSV file of their own, those that fit their share too, so that every connection's rows are whole in a file; if the sections, which then only name their files, still take more than the room, they go to one more file, a line each, whose summary takes their place.
- A connection that fails adds a line after the rows, with its error as a call on it alone would show it, such as `prod-app: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)`. The lines share half of the 10 KB and the rows take the rest: a line longer than its share is cut in the middle, as a long [error](#errors) is. The call fails only when every connection does, and the lines then share all of it.

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

The 5 top nodes are ranked by what each takes on its own: its total less its children's. That is cost without `analyze`, and time with it, summed over loops and parallel workers. Rows are per loop, as in EXPLAIN, which gives them two decimals from PostgreSQL 18. A `LIMIT` stops early the nodes that pass it rows as it reads them, down to the first that reads all of its input before returning a row, such as a sort, hash, plain or hashed aggregate, or a window aggregate over the whole partition, such as `count(*) OVER ()`, whose startup cost PostgreSQL 17 and later plan to cover its input. Earlier versions do not, so there only `analyze` shows the full scan under such a window. Without `analyze`, each of them counts the part of its estimated cost and rows that the `LIMIT` is planned to read, as EXPLAIN costs the `Limit` itself: under `LIMIT 10`, a seq scan of 20,000 rows shows `cost 0.26, ~10 rows`, not `cost 528, ~20000 rows`. The rows an `OFFSET` skips count in full, even under an outer `LIMIT`, and a `LIMIT` whose count is only known when the query runs, such as `LIMIT (SELECT 5)`, is planned to read a tenth of its input. An `EXISTS` over a subquery with no aggregate, window or set-returning function, set operation or `OFFSET` reads it only up to the first row, each time it runs, and its nodes count that part, as under a `LIMIT 1`: a seq scan of 20,000 rows of which 5,000 match shows `cost 0.12, ~1 row`. With `analyze` too, what a `LIMIT` or an `EXISTS` is planned to stop early counts as stopped: a `LIMIT` that returned fewer rows than planned may have run out of rows or been stopped by what reads it, such as an `EXISTS` or an outer `LIMIT`, and EXPLAIN does not say which. So a scan under a `LIMIT` that ran out of rows is not flagged for returning too few, though it read all of its input; a seq scan is still flagged as a full scan. The flags are:

- a seq scan that reads over 10,000 rows of a relation with over 10,000 (`pg_class.reltuples`): with `analyze`, the rows it returned and those its filter removed; without, all of the relation, or the part a `LIMIT` or an `EXISTS` is planned to read
- with `analyze`: a node that returned more than 10 times its estimated rows or, unless a `LIMIT` or an `EXISTS` is planned to stop it early, less than a tenth of them
- with `analyze`: a sort, hash or hash aggregate that spilled to disk and, from PostgreSQL 18, which reports it, a window aggregate, materialize or CTE scan that kept its rows on disk
- a nested loop whose outer side returns over 10,000 rows, so that its inner side runs that many times

Error positions point into `sql` as written. A summary over 10 KB is written to a spill file, one line per row, like a large `pg_query` result.

## Confirmation

A connection with [`confirm`](configuration.md#per-connection-options) set asks the user before a statement runs on it: before each `pg_query`, and each `pg_explain` with `analyze`, which runs the statement. The client shows a form with no fields that names the tool, the connections and the statement, with its `params` and, on each connection where the server sets one, the `search_path` that resolves its unqualified names, quoted as Postgres takes it:

```
Allow pg_query to run this statement on prod-app?

SELECT count(*) FROM orders WHERE status = $1

params: ["paid"]
search_path on prod-app: "app"
```

- Accepted, the call goes on as it would without `confirm`, role check included. Declined or cancelled, it fails with `declined by the user`, and nothing runs.
- The question comes once the arguments, the guard and the `search_path` check have passed, and before the role check: a statement they reject is not asked about.
- A character a terminal would act on or hide, a control character other than newline and tab or a formatting character such as a bidi control, is shown as an escape, such as `\x1b` or `\u202e`, so the statement shown is the statement that runs.
- A fan-out asks once, naming the connections in `connections` that set `confirm`, except those whose `search_path` is refused: they fail as they would alone. Declined, the statement runs on none of them.
- The client must support form elicitation. Without it, the call is refused before anything runs: `pg_query on prod-app requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation`.
- From protocol revision 2026-07-28 the question comes back as the result of the call, and the client calls again with the answer; before it, the server asks the client during the call, which it can only do over stdio and for a client that declared form elicitation when it connected, whatever a call's `_meta` claims: over [`--http`](configuration.md#streamable-http), an older client is refused as one without form elicitation is. An answer holds only for the question it answers: a call that brings it back with another statement, other `params`, other connections or another `search_path` asks again.
- The call writes its audit line once answered, `declined` when the user declines or cancels. A client that fails to ask ends the call with its own error, and one on revision 2026-07-28 may never call back with the answer: either way nothing runs, and no audit line is written.
- The other tools do not ask: they run only SQL the server writes.

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

When `pg_stats` has nothing on the columns, because the table was never analyzed, row-level security hides its statistics from the role or they are virtual generated columns (PostgreSQL 18), which ANALYZE skips, `pg_profile` samples the table instead, at most 10,000 rows taken with `TABLESAMPLE SYSTEM`, and counts each column's nulls and distinct values in the sample:

```
public.fresh: sampled 300 rows with TABLESAMPLE SYSTEM (100) LIMIT 10000, since pg_stats has nothing on these columns (never analyzed, hidden by row-level security, or virtual generated columns, which ANALYZE skips)
column	type	nulls	distinct
id	integer	0	300
note	text	100	7
(2 rows)
```

Both read as the connection's role: `pg_stats` shows only the columns the role can `SELECT`, and `pg_profile` says so when that leaves none; the sample is subject to privileges and row-level security. Relations the guard denies, such as `pg_authid`, are refused, since their statistics hold their values. Unlike `pg_query`, it does not cut values at 200 characters: five `uuid`s among the most common values alone take more. Output over 10 KB is written to a file, as `pg_query`'s is.

## Compare a schema across connections

`pg_diff_schema` compares one schema on two connections, typically production and staging. It takes `left` and `right`, two connection names, and `schema` (default `public`), and reads each side with one catalog query:

```
schema app, prod → staging: 5 differences (- only in prod, + only in staging)
+ table coupons
~ column orders.total: numeric(12,2) NOT NULL → numeric(14,2) NOT NULL
+ index orders.orders_placed_at_idx
~ view paid_orders: SELECT id, total FROM app.orders WHERE status = 'paid'::app.order_status; → SELECT id, total FROM app.orders WHERE status = 'paid'::app.order_status AND total > 0::numeric;
- table tenant_notes
```

`~` marks what both define differently, left then right. It compares relations (their kind, whether row-level security is on and the query of a view or materialized view), columns (type, `NOT NULL`, default, identity or generation), constraints, indexes and RLS policies; an index that implements a constraint shows as the constraint. Relations come in name order, each followed by its query, columns, constraints, indexes and policies, and a relation only one side has takes one line. A query is compared only when both sides have one: a view that became a table shows as a change of kind. When nothing differs, the result says `identical` and how many relations it compared.

- Only what each connection's role can read is compared, as in `pg_schema`, and partitions are left out.
- A `schema` over 63 bytes is refused: Postgres keeps no longer name, and would cut it to its first 63 bytes, which may name another schema.
- When reading one side fails, the error starts with that side's name: for a schema the side lacks or whose role lacks `USAGE` on it, and for a Postgres error, such as `staging: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)`.
- Each side's structure is read as one catalog value, which [`max_bytes`](configuration.md#defaults) bounds like any result: a table of 7 columns, 2 constraints and an index takes about 660 bytes, so the default 1 MiB holds about 1,600 of them. A larger schema fails with `staging: read the structure of schema "public": catalog result exceeds the configured limits (max_bytes=1048576); raise defaults.max_bytes in config.json to read larger schemas`.
- Definitions and queries are compared as each server prints them, so servers of different major versions can differ in spelling alone: in the query of a view that reads one table, Postgres 15 qualifies the column names and later versions do not, for one.
- Output past 10 KB is written to a CSV file, one difference per row, and summarized under the first line.
- Both connections go through the role check, and the audit line names both: `"connection":"prod,staging"`. Under `role_check: strict`, the error names the side that is refused or whose role check fails, as in `staging: role check failed, and role_check strict refuses connections it cannot check: …`.

## Check server health

`pg_health` (`connection`) reads the statistics views and catalogs and returns one row per finding, in the configured format, spilled to a file like a query result when it is too large:

```
section	detail
connections	up to 7 of max_connections 100: active 1, idle in transaction 1; the type and state of 5 are not visible to role app_reader
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
| `connections` | Client backends, which `max_connections` limits, by state; autovacuum and parallel workers have slots of their own |
| `long_running` | Client sessions, other than idle ones, whose transaction or query started over a minute ago, counted from whichever started first: pid, database, state, duration, wait event, application name. An open transaction keeps its locks, so a session in one is listed while it idles or runs a later query, and its line then says `transaction open for` and the transaction's age. One `idle in transaction (aborted)` holds none, and counts from its last query |
| `blocked` | Sessions that `pg_blocking_pids` reports waiting for a lock, and the pids blocking them |
| `cache_hit` | The share of this database's block reads found in shared buffers |
| `dead_tuples` | The 5 tables of this database with the most dead rows, with their share of the table's rows and their last autovacuum |
| `unused_indexes` | This database's indexes not scanned since the statistics were reset, largest first; unique indexes, primary keys and exclusion constraints' indexes are left out |
| `xid_wraparound` | The oldest `age(datfrozenxid)` of any database, against `autovacuum_freeze_max_age` |
| `replication` | On a replica, how long ago it replayed its last transaction, and how many received WAL bytes it has yet to replay |

`long_running`, `blocked` and `unused_indexes` stop at 10 rows and say how many more there are. Sessions are those of the whole server, and their query text is never shown: it can hold secrets. A role without the privileges of `pg_read_all_stats` sees the type, state, timing and wait event of its own sessions only, as above: `connections` then also counts the other backends connected to a database, which may be workers rather than client backends, and says `up to`. A section whose view or function the role may not use says `not visible to role <name>`, and the other sections still run. Unlike `pg_query`, `pg_health` does not cut values at 200 characters: an unused index with long schema, table and index names would lose its table and size.

## Mention tables as resources

Every table, view, materialized view and foreign table is also an MCP resource, `pgglass://<connection>/<schema>/<table>`, each name percent-encoded as in `pgglass://my%20db/app/caf%C3%A9`. In Claude Code, type `@` and pick one, such as `@pgglass:pgglass://prod-app/app/orders`, to add the relation to the prompt as `pg_schema` describes it, in `text/plain`.

- `resources/list` names, for each connection a tool call or resource read has used since the server started, the first 200 relations its role can read, by schema and then name: `prod-app: app.orders`, described by its kind, `table`. So a listing opens no connection the agent has not used, and before the first call it names nothing. A connection that fails, or that `role_check: strict` refuses, is left out. Each connection's list is cached for 5 minutes.
- When a call first uses a connection, the server sends `notifications/resources/list_changed`, so a client that listens for it lists the resources again. Over [`--http`](configuration.md#streamable-http), only a client on revision 2026-07-28 or later, which listens on a stream it keeps open, receives it.
- The resource template `pgglass://{connection}/{schema}/{table}` reads any relation, listed or not.
- A read goes through the role check and keeps to 10 KB, as `pg_schema` does: when it is the first response on a connection, the role check's findings start it. A listing cannot show them, so it leaves them to the next response.
- An unknown connection, or a schema or relation that does not exist or that the role cannot read, is a resource-not-found error (code `-32602`, with the URI in its data, cut in the middle where it would take the error past 10 KB). A connection that `role_check: strict` refuses, or a database error, is an internal error (`-32603`) whose message is the error a tool would return.

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
- `role check failed, and role_check strict refuses connections it cannot check: `, followed by why, such as a Postgres error
- invalid arguments, such as an unknown connection or argument name, or a name over 63 bytes
- `declined by the user` when the user declines or cancels a [confirmation](#confirmation), and `pg_query on prod requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation` when the client cannot ask

An error, with the role check's findings that may start it, keeps to the 10 KB of a response. A Postgres error can quote a value whole, and an unknown connection's lists every configured name: one longer than that is cut in the middle, keeping its start and its end, where the SQLSTATE, position, detail and hint are, as in `ERROR: invalid input syntax for type integer: "yyyy…[10088 bytes cut]…yyyy" (SQLSTATE 22P02)`. A call to a tool the server does not have is a protocol error instead, `unknown tool "…"` (code `-32602`), whose name is cut the same way.

## Audit log

Each tool call writes one JSON line to stderr, or one per connection for a [fan-out](#fan-out), once the user answers the [confirmation](#confirmation) it may ask for:

```json
{"time":"2026-10-12T14:59:12.2-03:00","level":"INFO","msg":"tool call","tool":"pg_query","connection":"prod-app","sql_sha256":"46880a16e933","sql_len":30,"duration_ms":41,"rows":1,"truncated":false,"outcome":"ok","sqlstate":""}
```

`connection` names both connections of a `pg_diff_schema` call, `prod,staging`. `sql_sha256` is the first 12 hex digits of the statement's SHA-256, so repeated statements can be matched without logging them; it is empty, and `sql_len` 0, for tools that take no SQL. `outcome` is `ok`, `guard_reject`, `db_error` (with `sqlstate` when Postgres raised it), `preflight_block`, `error` (for example, invalid arguments) or `declined` (the user declined or cancelled a [confirmation](#confirmation)). SQL text, parameters and values are never logged.

[Resources](#mention-tables-as-resources) are audited too: each read writes a line whose `tool` is `resource`, whether or not its URI names a table, and each listing one line per connection it lists, whose `tool` is `resource_list`, with the relations it listed as `rows`.

## Parallel calls

Every connection has its own pool, so calls do not wait on each other, on one connection or across several:

> "Compare the row count of the orders table between staging and production"

The agent can make two `pg_query` calls, which run at the same time, or one with `connections` (see [Fan-out](#fan-out)).

## Tips

- **Start with `pg_schema` or `pg_search`.** Agents write better SQL when they know the exact names and types.
- **Set `default_schema`** so unqualified names resolve without a `schema` argument.
- **Aggregate in SQL.** A `count(*)` or `GROUP BY` answers in one row where reading rows spills a file.
- **Look before you scan.** `pg_explain` plans a query without running it, and `pg_profile` reads what a table's columns hold from `pg_stats`.
