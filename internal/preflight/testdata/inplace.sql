-- Fixture for the functions that write in place, past READ ONLY and the
-- rollback: pg_surgery's heap_force_kill and heap_force_freeze, pg_visibility's
-- pg_truncate_visibility_map, and a look-alike of pg_log_standby_snapshot,
-- which only PostgreSQL 16 and later have. The role is cluster-wide and
-- outlives this database, so it is created only once.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pf_inplace') THEN
        CREATE ROLE pf_inplace LOGIN PASSWORD 'pf_inplace';
    END IF;
END $$;

-- Superusers alone may execute these, unless granted.
CREATE EXTENSION pg_surgery;
CREATE EXTENSION pg_visibility;
GRANT EXECUTE ON FUNCTION heap_force_kill(regclass, tid[]), heap_force_freeze(regclass, tid[]),
    pg_truncate_visibility_map(regclass) TO pf_inplace;

CREATE SCHEMA wal;
CREATE FUNCTION wal.pg_log_standby_snapshot() RETURNS pg_lsn LANGUAGE sql AS $$ SELECT '0/0'::pg_lsn $$;
GRANT USAGE ON SCHEMA wal TO pf_inplace;
