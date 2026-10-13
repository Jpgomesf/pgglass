-- Fixture for what PostgreSQL 18 adds to the audit: membership in the
-- predefined role pg_signal_autovacuum_worker, which older servers lack, and
-- EXECUTE on look-alikes of pg_logicalinspect's functions, which read logical
-- decoding snapshot files from the server. The role is cluster-wide and
-- outlives this database, so it is created only once.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pf_pg18') THEN
        CREATE ROLE pf_pg18 LOGIN PASSWORD 'pf_pg18';
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'pg_signal_autovacuum_worker') THEN
        GRANT pg_signal_autovacuum_worker TO pf_pg18;
    END IF;
END $$;

CREATE SCHEMA inspect;
CREATE FUNCTION inspect.pg_get_logical_snapshot_info(filename text) RETURNS text LANGUAGE sql AS $$ SELECT filename $$;
CREATE FUNCTION inspect.pg_get_logical_snapshot_meta(filename text) RETURNS text LANGUAGE sql AS $$ SELECT filename $$;
GRANT USAGE ON SCHEMA inspect TO pf_pg18;
