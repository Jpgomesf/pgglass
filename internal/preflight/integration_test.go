package preflight_test

import (
	_ "embed" // fixture SQL
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

//go:embed testdata/fixture.sql
var fixtureSQL string

// dbFixture holds testdata/fixture.sql: the roles pf_powers, pf_writer and pf_caller.
const dbFixture = "preflight"

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
		t.Run(tt.name, func(t *testing.T) {
			report, err := preflight.Audit(t.Context(), testdb.Querier(t, tt.role, tt.database))
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
