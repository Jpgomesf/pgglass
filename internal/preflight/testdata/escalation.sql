-- Fixture for the preflight findings that testdata/fixture.sql does not
-- cover, one role per area. Roles are cluster-wide and outlive this database,
-- so they are created only once.
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['pf_shadowed', 'pf_escapes', 'pf_root', 'pf_climber', 'pf_switcher', 'pf_quiet'] LOOP
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

-- pf_escapes: EXECUTE on look-alikes of the aws_s3, aws_lambda and
-- pg_background extensions (PUBLIC's default), and on pg_read_binary_file and
-- pg_log_backend_memory_contexts.
CREATE SCHEMA aws_s3;
CREATE FUNCTION aws_s3.query_export_to_s3(query text) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE SCHEMA aws_lambda;
CREATE FUNCTION aws_lambda.invoke(function_name text) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE SCHEMA bg;
CREATE FUNCTION bg.pg_background_launch(command text) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
GRANT USAGE ON SCHEMA aws_s3, aws_lambda, bg TO pf_escapes;
GRANT EXECUTE ON FUNCTION pg_read_binary_file(text), pg_log_backend_memory_contexts(integer) TO pf_escapes;

-- pf_climber: a member of the superuser pf_root, without inheriting from it.
-- Superuser is never inherited anyway, but SET ROLE reaches it.
ALTER ROLE pf_root NOLOGIN SUPERUSER;
ALTER ROLE pf_climber NOINHERIT;
GRANT pf_root TO pf_climber;

-- pf_switcher: a superuser that logs in as the role pf_quiet, which has
-- nothing to report; RESET ROLE returns to pf_switcher.
ALTER ROLE pf_switcher SUPERUSER;
ALTER ROLE pf_switcher SET role = 'pf_quiet';
