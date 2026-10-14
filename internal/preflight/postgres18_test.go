package preflight_test

import (
	_ "embed"
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

//go:embed testdata/postgres18.sql
var postgres18SQL string

// PostgreSQL 18's pg_signal_autovacuum_worker role and pg_logicalinspect
// functions are warnings; on older servers the audit skips the role.
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
