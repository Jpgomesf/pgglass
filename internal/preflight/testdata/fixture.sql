-- Fixture for the preflight findings that the shared testdb fixtures do not
-- cover, one role per area. Roles are cluster-wide and outlive this database,
-- so they are created only once.
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['pf_powers', 'pf_writer', 'pf_caller'] LOOP
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I LOGIN PASSWORD %L', r, r);
        END IF;
    END LOOP;
END $$;

-- pf_powers: every audited role attribute and predefined role. It does not
-- inherit the predefined roles, but SET ROLE still reaches them.
ALTER ROLE pf_powers BYPASSRLS CREATEROLE CREATEDB NOINHERIT;
GRANT pg_execute_server_program, pg_write_server_files, pg_read_server_files, pg_signal_backend TO pf_powers;

-- pf_writer: write privileges on 11 relations of every audited kind.
CREATE EXTENSION postgres_fdw;
CREATE SERVER elsewhere FOREIGN DATA WRAPPER postgres_fdw;
CREATE SCHEMA w;
GRANT USAGE ON SCHEMA w TO pf_writer;
CREATE TABLE w.a_insert (id int, note text);
CREATE TABLE w.b_update_column (id int, note text);
CREATE TABLE w.c_delete (id int);
CREATE TABLE w.d_truncate (id int);
CREATE TABLE w.e_partitioned (id int) PARTITION BY RANGE (id);
CREATE VIEW w.f_view AS SELECT id, note FROM w.a_insert;
CREATE MATERIALIZED VIEW w.g_owned AS SELECT 1 AS one;
CREATE FOREIGN TABLE w.h_foreign (id int) SERVER elsewhere;
CREATE TABLE w.i_all (id int);
CREATE TABLE w.j_insert (id int);
CREATE TABLE w.k_insert (id int);
GRANT INSERT ON w.a_insert, w.e_partitioned, w.h_foreign, w.j_insert, w.k_insert TO pf_writer;
GRANT UPDATE (note) ON w.b_update_column TO pf_writer;
GRANT DELETE ON w.c_delete TO pf_writer;
GRANT TRUNCATE ON w.d_truncate TO pf_writer;
GRANT INSERT, UPDATE ON w.f_view TO pf_writer;
ALTER MATERIALIZED VIEW w.g_owned OWNER TO pf_writer;
GRANT ALL ON w.i_all TO pf_writer;
-- Not findings: read only, a sequence, and a table in a schema pf_writer cannot use.
CREATE TABLE w.select_only (id int);
GRANT SELECT ON w.select_only TO pf_writer;
CREATE SEQUENCE w.counter;
GRANT ALL ON SEQUENCE w.counter TO pf_writer;
CREATE SCHEMA locked;
CREATE TABLE locked.t (id int);
GRANT ALL ON locked.t TO pf_writer;

-- pf_caller: EXECUTE on an http* function (PUBLIC's default) and on
-- lo_export, lo_import and pg_read_file.
CREATE SCHEMA fx;
GRANT USAGE ON SCHEMA fx TO pf_caller;
CREATE FUNCTION fx."http get"(url text) RETURNS text LANGUAGE sql AS $$ SELECT url $$;
GRANT EXECUTE ON FUNCTION lo_export(oid, text), lo_import(text), pg_read_file(text) TO pf_caller;
-- Not findings: '_' in lo_export is no wildcard, EXECUTE revoked, and a
-- schema pf_caller cannot use.
CREATE FUNCTION fx.loxexport() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE FUNCTION fx.http_revoked() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
REVOKE EXECUTE ON FUNCTION fx.http_revoked() FROM PUBLIC;
CREATE SCHEMA hidden;
CREATE FUNCTION hidden.dblink_hidden() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
