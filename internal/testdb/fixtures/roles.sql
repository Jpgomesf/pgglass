-- Cluster-wide login roles used by the fixture databases.
-- Each role's password is its name (see testdb.DSN).

-- Only USAGE on schema app and SELECT on its relations (granted in app.sql).
-- PUBLIC keeps its defaults: CONNECT and TEMP on databases, USAGE on schema public.
CREATE ROLE ro_clean LOGIN PASSWORD 'ro_clean';

-- Like ro_clean, plus INSERT on app.audit_log, EXECUTE on the dblink functions
-- (granted in app.sql) and membership in pg_read_server_files.
CREATE ROLE ro_leaky LOGIN PASSWORD 'ro_leaky';
GRANT pg_read_server_files TO ro_leaky;

-- SELECT on public.profiles in supabase_like; PUBLIC grants there do the rest.
CREATE ROLE ro_supabase LOGIN PASSWORD 'ro_supabase';
