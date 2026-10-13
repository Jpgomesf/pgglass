package health

// The statistics queries. Each returns one row holding one JSON value whose
// keys are the names of the Go fields it decodes into (see render.go). Ties
// are broken by pid or by name, whose C collation makes the order the same
// on every server. None reads pg_stat_activity.query: other sessions' SQL can
// hold secrets. The lists stop at 10 items and say how many there are in all.

// viewerSQL returns the current role and whether it has the privileges of
// pg_read_all_stats. Without them, pg_stat_activity shows the type, state,
// timing and wait event of the role's own sessions only, and NULL for the
// others.
const viewerSQL = `SELECT json_build_object('Role', current_user, 'AllStats', pg_has_role('pg_read_all_stats', 'USAGE'))`

// connectionsSQL counts the client backends, which max_connections limits, by
// state: autovacuum, parallel and other background workers have slots of
// their own. To a role without the privileges of pg_read_all_stats, the
// backends that are not its own have no type, so those connected to a
// database count too, with no state.
const connectionsSQL = `
SELECT json_build_object(
	'Max', current_setting('max_connections')::int,
	'Used', coalesce(sum(n), 0),
	'States', coalesce(json_agg(json_build_object('State', state, 'Count', n) ORDER BY n DESC, state), '[]'))
FROM (
	SELECT state, count(*) AS n
	FROM pg_stat_activity
	WHERE backend_type = 'client backend' OR (backend_type IS NULL AND datid IS NOT NULL)
	GROUP BY state
) s`

// longRunningSQL lists the client sessions, other than idle ones, whose
// current or last query started more than $1 seconds ago or, for a session
// idle in transaction, whose transaction did, longest first. An open
// transaction keeps its locks however recent its last query. One that failed
// leaves its session idle in transaction (aborted), with no locks and no
// xact_start.
const longRunningSQL = `
SELECT json_build_object('Total', coalesce(max(total), 0), 'Items', coalesce(json_agg(json_build_object(
		'PID', pid, 'Database', datname, 'State', state, 'Seconds', seconds, 'Wait', wait, 'App', application_name)
	ORDER BY seconds DESC, pid), '[]'))
FROM (
	SELECT pid, datname, state, extract(epoch FROM now() - since)::bigint AS seconds,
		concat_ws(':', wait_event_type, wait_event) AS wait, application_name, count(*) OVER () AS total
	FROM (
		SELECT pid, datname, state, wait_event_type, wait_event, application_name,
			CASE state WHEN 'idle in transaction' THEN xact_start ELSE query_start END AS since
		FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND state <> 'idle'
	) a
	WHERE since < now() - $1::int * interval '1 second'
	ORDER BY seconds DESC, pid
	LIMIT 10
) s`

// blockedSQL lists the sessions that pg_blocking_pids reports as waiting for
// a lock, with the pids that block them, longest-running first. It finds
// them whatever the role, since pg_blocking_pids reads the lock table, but
// their state, timing and wait event are NULL to a role that may not see
// them. Postgres does not pull up a subquery that calls a volatile function,
// so pg_blocking_pids runs once per session.
const blockedSQL = `
SELECT json_build_object('Total', coalesce(max(total), 0), 'Items', coalesce(json_agg(json_build_object(
		'PID', pid, 'Database', datname, 'State', state, 'Seconds', seconds, 'Wait', wait, 'App', application_name,
		'BlockedBy', blocked_by)
	ORDER BY seconds DESC NULLS LAST, pid), '[]'))
FROM (
	SELECT pid, datname, state, extract(epoch FROM now() - query_start)::bigint AS seconds,
		concat_ws(':', wait_event_type, wait_event) AS wait, application_name, blocked_by, count(*) OVER () AS total
	FROM (
		SELECT pid, datname, state, query_start, wait_event_type, wait_event, application_name,
			pg_blocking_pids(pid) AS blocked_by
		FROM pg_stat_activity
	) a
	WHERE cardinality(blocked_by) > 0
	ORDER BY seconds DESC NULLS LAST, pid
	LIMIT 10
) s`

// cacheHitSQL returns the blocks of the current database found in shared
// buffers, and those read from the operating system.
const cacheHitSQL = `
SELECT (SELECT json_build_object('Database', datname, 'Hit', blks_hit, 'Read', blks_read)
	FROM pg_stat_database
	WHERE datname = current_database())`

// deadTuplesSQL lists the 5 tables with the most dead rows, their live rows,
// and how long ago autovacuum last processed each. Ranked by their share of
// dead rows instead, a tiny table with a few would crowd out large ones.
const deadTuplesSQL = `
SELECT coalesce(json_agg(json_build_object(
		'Name', format('%I.%I', schemaname, relname), 'Dead', n_dead_tup, 'Live', n_live_tup,
		'AutovacuumSecondsAgo', extract(epoch FROM now() - last_autovacuum)::bigint)
	ORDER BY n_dead_tup DESC, schemaname, relname), '[]')
FROM (
	SELECT schemaname, relname, n_dead_tup, n_live_tup, last_autovacuum
	FROM pg_stat_user_tables
	WHERE n_dead_tup > 0
	ORDER BY n_dead_tup DESC, schemaname, relname
	LIMIT 5
) t`

// unusedIndexesSQL lists the indexes never scanned since the statistics were
// reset, largest first. Unique indexes, primary keys among them, and those
// of exclusion constraints are left out: they enforce a constraint, scanned
// or not.
const unusedIndexesSQL = `
SELECT json_build_object('Total', coalesce(max(total), 0), 'Items', coalesce(json_agg(json_build_object(
		'Name', format('%I.%I', schemaname, indexrelname), 'Table', format('%I.%I', schemaname, relname),
		'Size', pg_size_pretty(bytes))
	ORDER BY bytes DESC, schemaname, indexrelname), '[]'))
FROM (
	SELECT s.schemaname, s.indexrelname, s.relname, pg_relation_size(s.indexrelid) AS bytes, count(*) OVER () AS total
	FROM pg_stat_user_indexes s
	JOIN pg_index i ON i.indexrelid = s.indexrelid
	WHERE s.idx_scan = 0 AND NOT i.indisunique AND NOT i.indisexclusion
	ORDER BY bytes DESC, s.schemaname, s.indexrelname
	LIMIT 10
) u`

// wraparoundSQL returns the database with the oldest datfrozenxid, its age,
// and autovacuum_freeze_max_age, the age at which autovacuum freezes a
// database's tables even when it is turned off.
const wraparoundSQL = `
SELECT json_build_object('Database', datname, 'Age', age(datfrozenxid),
	'FreezeMaxAge', current_setting('autovacuum_freeze_max_age')::int)
FROM pg_database
ORDER BY age(datfrozenxid) DESC, datname
LIMIT 1`

// replicationSQL returns whether the server is a replica and, on a replica,
// how long ago it replayed its last transaction and how many bytes of the WAL
// it received it has yet to replay. Those functions return NULL on a primary.
const replicationSQL = `
SELECT json_build_object(
	'Replica', pg_is_in_recovery(),
	'ReplayedSecondsAgo', extract(epoch FROM now() - pg_last_xact_replay_timestamp())::bigint,
	'UnreplayedBytes', pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::bigint)`
