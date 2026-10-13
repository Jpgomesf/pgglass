package preflight_test

import (
	_ "embed" // fixture SQL
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

//go:embed testdata/inplace.sql
var inplaceSQL string

// EXECUTE on a function that writes in place, past READ ONLY and the
// rollback, is critical, so role_check strict refuses the role.
func TestAuditFlagsFunctionsThatWriteInPlace(t *testing.T) {
	const database = "preflight_inplace"
	testdb.CreateDatabase(t, database, inplaceSQL)
	report, err := preflight.Audit(t.Context(), testdb.Querier(t, "pf_inplace", database))
	if err != nil {
		t.Fatal(err)
	}
	wantFindings(t, report, critical("function_execute", "EXECUTE on 4 functions: public.heap_force_freeze, "+
		"public.heap_force_kill, public.pg_truncate_visibility_map, wal.pg_log_standby_snapshot"))
	if !report.Blocking() {
		t.Error("Blocking() = false: role_check strict would admit this role")
	}
}
