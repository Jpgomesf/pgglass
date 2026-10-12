-- Fixture for the ways past a read-only audit that testdata/fixture.sql does
-- not cover, one role per way. Roles are cluster-wide and outlive this
-- database, so they are created only once.
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['pf_shadowed'] LOOP
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I LOGIN PASSWORD %L', r, r);
        END IF;
    END LOOP;
END $$;

-- pf_shadowed: a member of pg_execute_server_program whose search_path puts
-- a pg_roles look-alike that hides the predefined roles before pg_catalog.
GRANT pg_execute_server_program TO pf_shadowed;
CREATE SCHEMA shadow;
CREATE VIEW shadow.pg_roles AS SELECT * FROM pg_catalog.pg_roles WHERE rolname NOT LIKE 'pg\_%';
GRANT USAGE ON SCHEMA shadow TO pf_shadowed;
GRANT SELECT ON shadow.pg_roles TO pf_shadowed;
ALTER ROLE pf_shadowed SET search_path = shadow, pg_catalog;
