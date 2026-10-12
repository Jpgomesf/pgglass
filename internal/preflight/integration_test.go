package preflight_test

import (
	"context"
	_ "embed" // fixture SQL
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/engine/enginetest"
	"github.com/jpgomesf/mcp-postgres/internal/preflight"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

//go:embed testdata/fixture.sql
var fixtureSQL string

// dbFixture holds testdata/fixture.sql: the roles pf_powers, pf_writer and pf_caller.
const dbFixture = "preflight"

// The engine may use either pgx protocol, so every audit runs under both.
var queriers = []struct {
	name string
	new  func(testing.TB, string) engine.Querier
}{
	{"simple protocol", enginetest.Querier},
	{"extended protocol", extendedQuerier},
}

func TestAudit(t *testing.T) {
	testdb.CreateDatabase(t, dbFixture, fixtureSQL)
	tests := []struct {
		name, role, database string
		blocking             bool
		want                 []preflight.Finding
	}{
		{"superuser", testdb.Superuser, testdb.DBApp, true, []preflight.Finding{
			critical("superuser", "bypasses every privilege check"),
		}},
		{"clean", testdb.RoleClean, testdb.DBApp, false, nil},
		{"leaky", testdb.RoleLeaky, testdb.DBApp, true, []preflight.Finding{
			critical("function_execute", "EXECUTE on 21 functions, first 10: public.dblink, "+
				"public.dblink_build_sql_delete, public.dblink_build_sql_insert, public.dblink_build_sql_update, "+
				"public.dblink_cancel_query, public.dblink_close, public.dblink_connect, public.dblink_connect_u, "+
				"public.dblink_current_query, public.dblink_disconnect"),
			warning("role_membership", "member of pg_read_server_files"),
			warning("table_write", "INSERT on app.audit_log"),
		}},
		{"supabase", testdb.RoleSupabase, testdb.DBSupabase, true, []preflight.Finding{
			critical("function_execute", "EXECUTE on net.http_post"),
			warning("table_write", "INSERT, UPDATE, DELETE on net.http_request_queue"),
		}},
		{"attributes and predefined roles", "pf_powers", dbFixture, true, []preflight.Finding{
			critical("role_membership", "member of pg_execute_server_program"),
			critical("role_membership", "member of pg_write_server_files"),
			warning("bypassrls", "ignores row-level security"),
			warning("createrole", "can create roles"),
			warning("createdb", "can create databases"),
			warning("role_membership", "member of pg_read_server_files"),
			warning("role_membership", "member of pg_signal_backend"),
		}},
		{"table writes", "pf_writer", dbFixture, false, []preflight.Finding{
			warning("table_write", "11 relations, first 10: INSERT on w.a_insert; UPDATE on w.b_update_column; "+
				"DELETE on w.c_delete; TRUNCATE on w.d_truncate; INSERT on w.e_partitioned; INSERT, UPDATE on w.f_view; "+
				"INSERT, UPDATE, DELETE, TRUNCATE on w.g_owned; INSERT on w.h_foreign; "+
				"INSERT, UPDATE, DELETE, TRUNCATE on w.i_all; INSERT on w.j_insert"),
		}},
		{"function grants", "pf_caller", dbFixture, true, []preflight.Finding{
			critical("function_execute", `EXECUTE on 2 functions: fx."http get", pg_catalog.lo_export`),
			warning("function_execute", "EXECUTE on 2 functions: pg_catalog.lo_import, pg_catalog.pg_read_file"),
		}},
	}
	for _, tt := range tests {
		for _, querier := range queriers {
			t.Run(tt.name+"/"+querier.name, func(t *testing.T) {
				q := querier.new(t, testdb.DSN(t, tt.role, tt.database))
				report, err := preflight.Audit(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				if report.Role != tt.role {
					t.Errorf("role = %q, want %q", report.Role, tt.role)
				}
				wantFindings(t, report, tt.want...)
				if report.Blocking() != tt.blocking {
					t.Errorf("Blocking() = %v, want %v", report.Blocking(), tt.blocking)
				}
			})
		}
	}
}

// extendedQuerier is a test-only engine.Querier like enginetest.Querier, but
// over pgx's default extended protocol, asking for text results. Each query
// runs in its own READ ONLY transaction, always rolled back.
func extendedQuerier(t testing.TB, dsn string) engine.Querier {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return extended{conn: conn}
}

type extended struct {
	conn *pgx.Conn
}

func (e extended) Query(ctx context.Context, query engine.Query) (*result.Result, error) {
	tx, err := e.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	args := append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, query.Params...)
	rows, err := tx.Query(ctx, query.SQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &result.Result{Columns: make([]result.Column, len(rows.FieldDescriptions()))}
	for rows.Next() {
		values := make([]*string, len(rows.RawValues()))
		for i, v := range rows.RawValues() {
			if v != nil {
				s := string(v)
				values[i] = &s
			}
		}
		res.Rows = append(res.Rows, values)
	}
	return res, rows.Err()
}

// The test-only querier must hold the line the engine holds: read only.
func TestExtendedQuerierIsReadOnly(t *testing.T) {
	q := extendedQuerier(t, testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp)) // may INSERT into app.audit_log
	_, err := q.Query(t.Context(), engine.Query{SQL: "INSERT INTO app.audit_log (message) VALUES ($1)", Params: []any{"nope"}})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("INSERT error = %v, want read_only_sql_transaction (25006)", err)
	}
	res, err := q.Query(t.Context(), engine.Query{SQL: "SELECT count(*) FROM app.audit_log WHERE message = $1", Params: []any{"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got == nil || *got != "0" {
		t.Errorf("count = %v, want text 0", got)
	}
}
