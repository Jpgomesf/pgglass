package engine_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func cleanApp(t *testing.T, d config.Defaults) *engine.Manager {
	t.Helper()
	return newManager(t, d, oneConn(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)))
}

func TestValuesComeBackAsPostgresText(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{})
	res := mustQuery(t, m, engine.Query{SQL: `SELECT
		'0ddc4d69-0dcf-fba9-15c2-88a68e6665de'::uuid AS id,
		123456789012345678901234567890.123456789 AS big,
		12.30::numeric(12, 2) AS total,
		'{"b": [1, 2.50], "a": null}'::jsonb AS doc,
		ARRAY[1, NULL, 3] AS ints,
		ARRAY['a b', 'c"d', NULL] AS texts,
		'\xdeadbeef'::bytea AS raw,
		'2024-03-01 09:30:00.123456+00'::timestamptz AS at,
		'shipped'::app.order_status AS status,
		ARRAY['paid']::app.order_status[] AS statuses`})

	want := []struct{ name, typeName, value string }{
		{"id", "uuid", "0ddc4d69-0dcf-fba9-15c2-88a68e6665de"},
		{"big", "numeric", "123456789012345678901234567890.123456789"},
		{"total", "numeric", "12.30"},
		{"doc", "jsonb", `{"a": null, "b": [1, 2.50]}`},
		{"ints", "_int4", "{1,NULL,3}"},
		{"texts", "_text", `{"a b","c\"d",NULL}`},
		{"raw", "bytea", `\xdeadbeef`},
		{"at", "timestamptz", "2024-03-01 09:30:00.123456+00"},
		{"status", "order_status", "shipped"},
		{"statuses", "_order_status", "{paid}"},
	}
	if len(res.Columns) != len(want) || len(res.Rows) != 1 {
		t.Fatalf("columns %+v, %d rows", res.Columns, len(res.Rows))
	}
	for i, w := range want {
		if c := res.Columns[i]; c.Name != w.name || c.TypeName != w.typeName || c.TypeOID == 0 {
			t.Errorf("column %d = %+v, want %s of type %s", i, c, w.name, w.typeName)
		}
		if got := deref(res.Rows[0][i]); got != w.value {
			t.Errorf("%s = %s, want %s", w.name, got, w.value)
		}
	}
}

func TestDuplicateColumnsAndNullSurvive(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{})
	res := mustQuery(t, m, engine.Query{SQL: "SELECT NULL::text AS a, '' AS a, 'x' AS a"})
	if len(res.Columns) != 3 || res.Columns[0].Name != "a" || res.Columns[2].Name != "a" {
		t.Fatalf("columns = %+v", res.Columns)
	}
	row := res.Rows[0]
	if row[0] != nil || row[1] == nil || *row[1] != "" || deref(row[2]) != "x" {
		t.Errorf("row = [%s %q %s], want [NULL \"\" x]", deref(row[0]), deref(row[1]), deref(row[2]))
	}
}

func TestParamsBind(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{})

	res := mustQuery(t, m, engine.Query{
		SQL:    "SELECT $1::int + 1, $2::text, $3::bool, $4::text IS NULL, $5::numeric",
		Params: []any{float64(41), "x'); DROP TABLE app.customers; --", true, nil, 1.5},
	})
	want := []string{"42", "x'); DROP TABLE app.customers; --", "t", "t", "1.5"}
	for i, w := range want {
		if got := deref(res.Rows[0][i]); got != w {
			t.Errorf("$%d = %s, want %s", i+1, got, w)
		}
	}

	res = mustQuery(t, m, engine.Query{
		SQL:    "SELECT o.total, c.email FROM app.orders o JOIN app.customers c ON c.id = o.customer_id WHERE o.id = $1",
		Params: []any{float64(6)},
	})
	if got := deref(res.Rows[0][0]) + " " + deref(res.Rows[0][1]); got != "74.04 customer7@example.com" {
		t.Errorf("order 6 = %s", got)
	}
}

func TestSearchPathAppliesToOneQuery(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{})

	res := mustQuery(t, m, engine.Query{SQL: "SELECT count(*) FROM customers", SearchPath: "app"})
	if got := deref(res.Rows[0][0]); got != "50" {
		t.Errorf("count = %s, want 50", got)
	}
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT count(*) FROM customers"})
	wantSQLState(t, err, "42P01")

	odd := `app"; SET LOCAL search_path TO public; --`
	res = mustQuery(t, m, engine.Query{SQL: "SELECT current_setting('search_path')", SearchPath: odd})
	if got, want := deref(res.Rows[0][0]), `"app""; SET LOCAL search_path TO public; --"`; got != want {
		t.Errorf("search_path = %s, want the one quoted identifier %s", got, want)
	}
}

func TestLimitsAreSetPerTransaction(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{StatementTimeoutSeconds: 7, LockTimeoutSeconds: 3})
	res := mustQuery(t, m, engine.Query{SQL: `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
		current_setting('idle_in_transaction_session_timeout'), current_setting('transaction_read_only')`})
	for i, want := range []string{"7s", "3s", "7s", "on"} {
		if got := deref(res.Rows[0][i]); got != want {
			t.Errorf("setting %d = %s, want %s", i, got, want)
		}
	}
}

// config.MaxTimeoutSeconds is the largest timeout Postgres accepts; one
// second more fails every query in its setup.
func TestLargestTimeoutsAreAccepted(t *testing.T) {
	t.Parallel()
	const largest = config.MaxTimeoutSeconds
	m := cleanApp(t, config.Defaults{StatementTimeoutSeconds: largest, LockTimeoutSeconds: largest})
	res := mustQuery(t, m, engine.Query{SQL: `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
		current_setting('idle_in_transaction_session_timeout')`})
	for i := range 3 {
		if got := deref(res.Rows[0][i]); got != "2147483s" {
			t.Errorf("setting %d = %s, want 2147483s", i, got)
		}
	}

	over := cleanApp(t, config.Defaults{StatementTimeoutSeconds: largest + 1})
	_, err := over.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "22023")
}

func TestDSNCannotTurnOffReadOnlyDefault(t *testing.T) {
	t.Parallel()
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp) +
		"&default_transaction_read_only=off&options=-c%20default_transaction_read_only%3Doff"
	m := newManager(t, config.Defaults{}, dsn)
	if got := value(t, m, "SELECT current_setting('default_transaction_read_only')"); got != "on" {
		t.Errorf("default_transaction_read_only = %s, want on", got)
	}
}

func TestStatementTimeout(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{StatementTimeoutSeconds: 1})
	start := time.Now()
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT pg_sleep(5)"})
	wantSQLState(t, err, "57014")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timed out after %v, want about 1s", elapsed)
	}
}

func TestLockTimeout(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "engine_locks", "CREATE TABLE t (id int)")
	m := newManager(t, config.Defaults{StatementTimeoutSeconds: 10, LockTimeoutSeconds: 1},
		testdb.DSN(t, testdb.Superuser, "engine_locks"))

	locker := testdb.Conn(t, testdb.Superuser, "engine_locks")
	tx, err := locker.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "LOCK TABLE t IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT count(*) FROM t"})
	wantSQLState(t, err, "55P03")
}

func TestPostgresErrorsAreFlattened(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{})

	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 'x'::jsonb"})
	var qe *engine.QueryError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want a *QueryError", err)
	}
	want := engine.QueryError{SQLState: "22P02", Message: "invalid input syntax for type json", Detail: `Token "x" is invalid.`, Position: 8}
	if *qe != want {
		t.Errorf("QueryError = %+v, want %+v", *qe, want)
	}

	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT nam FROM app.customers"})
	if !errors.As(err, &qe) || qe.SQLState != "42703" || qe.Position != 8 || !strings.Contains(qe.Hint, `"customers.name"`) {
		t.Errorf("err = %+v, want 42703 at 8 with a hint naming customers.name", qe)
	}
}

func TestMaxRowsTruncates(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxRows: 10})

	res := mustQuery(t, m, engine.Query{SQL: "SELECT id FROM app.events ORDER BY id"})
	if len(res.Rows) != 10 || deref(res.Rows[9][0]) != "10" || !res.Truncated || res.TruncatedReason != "max_rows=10" {
		t.Errorf("%d rows, truncated %v %q; want 10 rows ending at id 10, max_rows=10", len(res.Rows), res.Truncated, res.TruncatedReason)
	}

	res = mustQuery(t, m, engine.Query{SQL: "SELECT g FROM generate_series(1, 10) g"})
	if len(res.Rows) != 10 || res.Truncated {
		t.Errorf("exactly max_rows: %d rows, truncated %v; want 10, not truncated", len(res.Rows), res.Truncated)
	}
}

func TestQueryMaxRowsOnlyLowersTheCap(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxRows: 10})
	for _, tc := range []struct {
		asked, wantRows int
		wantReason      string
	}{
		{3, 3, "max_rows=3"},
		{0, 10, "max_rows=10"},
		{50, 10, "max_rows=10"},
	} {
		res := mustQuery(t, m, engine.Query{SQL: "SELECT id FROM app.events", MaxRows: tc.asked})
		if len(res.Rows) != tc.wantRows || res.TruncatedReason != tc.wantReason {
			t.Errorf("MaxRows %d: %d rows, truncated %q; want %d, %s", tc.asked, len(res.Rows), res.TruncatedReason, tc.wantRows, tc.wantReason)
		}
	}
}

// Role and database defaults must not change how the server reads SQL that
// the guard has checked, nor the encoding results come back in.
func TestServerDefaultsCannotChangeHowSQLIsRead(t *testing.T) {
	t.Parallel()
	const name = "engine_lexing"
	testdb.CreateDatabase(t, name, "ALTER DATABASE "+name+" SET standard_conforming_strings = off;"+
		"ALTER DATABASE "+name+" SET client_encoding = 'LATIN1';")
	var scs, encoding string
	if err := testdb.Conn(t, testdb.Superuser, name).QueryRow(t.Context(), "SELECT current_setting('standard_conforming_strings'), current_setting('client_encoding')").
		Scan(&scs, &encoding); err != nil {
		t.Fatal(err)
	}
	if scs != "off" || encoding != "LATIN1" {
		t.Fatalf("plain session: standard_conforming_strings=%s client_encoding=%s, want off and LATIN1", scs, encoding)
	}

	m := newManager(t, config.Defaults{}, testdb.DSN(t, testdb.RoleClean, name))
	res := mustQuery(t, m, engine.Query{SQL: `SELECT current_setting('standard_conforming_strings'),
		current_setting('client_encoding'), 'x\', 'São Paulo'`})
	for i, want := range []string{"on", "UTF8", `x\`, "São Paulo"} {
		if got := deref(res.Rows[0][i]); got != want {
			t.Errorf("value %d = %q, want %q", i, got, want)
		}
	}
}

// A value counts its length plus 24 bytes against max_bytes.
func TestMaxBytesTruncates(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxBytes: 100})

	res := mustQuery(t, m, engine.Query{SQL: "SELECT repeat('x', 26) FROM generate_series(1, 10)"})
	if len(res.Rows) != 2 || !res.Truncated || res.TruncatedReason != "max_bytes=100" {
		t.Errorf("%d rows, truncated %v %q; want 2 rows, max_bytes=100", len(res.Rows), res.Truncated, res.TruncatedReason)
	}

	res = mustQuery(t, m, engine.Query{SQL: "SELECT repeat('x', 26) FROM generate_series(1, 2)"})
	if len(res.Rows) != 2 || res.Truncated {
		t.Errorf("exactly max_bytes: %d rows, truncated %v; want 2, not truncated", len(res.Rows), res.Truncated)
	}

	res = mustQuery(t, m, engine.Query{SQL: "SELECT '', NULL::text FROM generate_series(1, 10)"})
	if len(res.Rows) != 2 || res.TruncatedReason != "max_bytes=100" {
		t.Errorf("empty values: %d rows, truncated %q; want 2 rows, max_bytes=100", len(res.Rows), res.TruncatedReason)
	}
}

func running(t *testing.T, observer *pgx.Conn, fragment string) int {
	t.Helper()
	n, err := strconv.Atoi(testdb.Value[string](t, observer, `SELECT count(*)::text FROM pg_stat_activity
		WHERE state = 'active' AND pid <> pg_backend_pid() AND strpos(query, $1) > 0`, fragment))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A truncated result comes back without waiting for the rest of the query,
// which the server stops, and the connection stays usable.
func TestTruncationStopsTheQuery(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxRows: 3, StatementTimeoutSeconds: 10})
	pid := value(t, m, "SELECT pg_backend_pid()")

	const slow = "SELECT g, repeat('x', 10000), pg_sleep(0.005) FROM generate_series(1, 100000) AS g"
	start := time.Now()
	res := mustQuery(t, m, engine.Query{SQL: slow})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("truncated query took %v: the rest of the result was read", elapsed)
	}
	if len(res.Rows) != 3 || res.TruncatedReason != "max_rows=3" {
		t.Errorf("%d rows, truncated %q; want 3, max_rows=3", len(res.Rows), res.TruncatedReason)
	}
	if n := running(t, testdb.Conn(t, testdb.Superuser, testdb.DBApp), "generate_series(1, 100000)"); n != 0 {
		t.Errorf("%d backends still run the truncated query", n)
	}
	if got := value(t, m, "SELECT pg_backend_pid()"); got != pid {
		t.Errorf("backend %s after truncation, was %s: the connection was dropped", got, pid)
	}
}

// A context that ends cancels the query on the server, and the connection
// stays usable.
func TestEndedContextCancelsTheQuery(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{StatementTimeoutSeconds: 60})
	pid := value(t, m, "SELECT pg_backend_pid()")

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Query(ctx, db, engine.Query{SQL: "SELECT pg_sleep(30)"})
	var qe *engine.QueryError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &qe) {
		t.Errorf("err = %v, want a context deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v, want soon after the deadline", elapsed)
	}

	watch := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	eventually(t, "the server to stop pg_sleep(30)", func() bool { return running(t, watch, "pg_sleep(30)") == 0 })
	if got := value(t, m, "SELECT pg_backend_pid()"); got != pid {
		t.Errorf("backend %s after the cancel, was %s: the connection was dropped", got, pid)
	}
}

func TestParallelQueriesUseSeparateConnections(t *testing.T) {
	t.Parallel()
	m := newManager(t, config.Defaults{}, testdb.DSN(t, testdb.RoleClean, testdb.DBApp))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		pids = map[string]bool{}
	)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := m.Query(t.Context(), db, engine.Query{
				SQL:    "SELECT $1::int, pg_backend_pid(), count(*), pg_sleep(0.05) FROM app.events WHERE id % 20 = $1",
				Params: []any{i},
			})
			if err != nil {
				t.Errorf("query %d: %v", i, err)
				return
			}
			row := res.Rows[0]
			if deref(row[0]) != strconv.Itoa(i) || deref(row[2]) != "1000" {
				t.Errorf("query %d got [%s %s %s]", i, deref(row[0]), deref(row[1]), deref(row[2]))
			}
			mu.Lock()
			pids[deref(row[1])] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(pids) < 2 {
		t.Errorf("20 parallel queries ran on %d backend(s), want several", len(pids))
	}
}
