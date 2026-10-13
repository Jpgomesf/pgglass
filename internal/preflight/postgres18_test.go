package preflight_test

import (
	_ "embed"
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

//go:embed testdata/postgres18.sql
var postgres18SQL string

// PostgreSQL 18 adds pg_signal_autovacuum_worker, whose members signal
// autovacuum workers as pg_signal_backend's do other sessions, and
// pg_logicalinspect, whose functions read server files as pg_read_binary_file
// does. Both are warnings. Older servers lack the role, and the audit skips
// it.
func TestAuditWarnsOfWhatPostgreSQL18Adds(t *testing.T) {
	const database = "preflight_postgres18"
	testdb.CreateDatabase(t, database, postgres18SQL)
	report, err := preflight.Audit(t.Context(), testdb.Querier(t, "pf_pg18", database))
	if err != nil {
		t.Fatal(err)
	}
	want := []preflight.Finding{warning("function_execute",
		"EXECUTE on 2 functions: inspect.pg_get_logical_snapshot_info, inspect.pg_get_logical_snapshot_meta")}
	if testdb.ServerVersion(t) >= 180000 {
		want = append([]preflight.Finding{warning("role_membership", "member of pg_signal_autovacuum_worker")}, want...)
	}
	wantFindings(t, report, want...)
	if report.Blocking() {
		t.Error("Blocking() = true: warnings alone must not refuse the role")
	}
}
