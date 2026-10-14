package health

const viewerSQL = `SELECT json_build_object('Role', current_user, 'AllStats', pg_has_role('pg_read_all_stats', 'USAGE'))`

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

const longRunningSQL = `
SELECT json_build_object('Total', coalesce(max(total), 0), 'Items', coalesce(json_agg(json_build_object(
		'PID', pid, 'Database', datname, 'State', state, 'Seconds', seconds, 'FromTransaction', from_transaction,
		'Wait', wait, 'App', application_name)
	ORDER BY seconds DESC, pid), '[]'))
FROM (
	SELECT pid, datname, state, extract(epoch FROM now() - since)::bigint AS seconds, from_transaction,
		concat_ws(':', wait_event_type, wait_event) AS wait, application_name, count(*) OVER () AS total
	FROM (
		SELECT pid, datname, state, wait_event_type, wait_event, application_name,
			least(xact_start, query_start) AS since, xact_start < query_start AS from_transaction
		FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND state <> 'idle'
	) a
	WHERE since < now() - $1::int * interval '1 second'
	ORDER BY seconds DESC, pid
	LIMIT 10
) s`

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

const cacheHitSQL = `
SELECT (SELECT json_build_object('Database', datname, 'Hit', blks_hit, 'Read', blks_read)
	FROM pg_stat_database
	WHERE datname = current_database())`

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

const wraparoundSQL = `
SELECT json_build_object('Database', datname, 'Age', age(datfrozenxid),
	'FreezeMaxAge', current_setting('autovacuum_freeze_max_age')::int)
FROM pg_database
ORDER BY age(datfrozenxid) DESC, datname
LIMIT 1`

const replicationSQL = `
SELECT json_build_object(
	'Replica', pg_is_in_recovery(),
	'ReplayedSecondsAgo', extract(epoch FROM now() - pg_last_xact_replay_timestamp())::bigint,
	'UnreplayedBytes', pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::bigint)`
