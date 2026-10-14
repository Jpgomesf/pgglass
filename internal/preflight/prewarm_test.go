package preflight_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_prewarm lets PUBLIC execute autoprewarm_*, which write a server file, so
// ro_clean gets a critical finding, which role_check strict refuses.
func TestAuditFlagsAutoprewarm(t *testing.T) {
	const database = "preflight_prewarm"
	testdb.CreateDatabase(t, database, "CREATE EXTENSION pg_prewarm")
	report, err := preflight.Audit(t.Context(), testdb.Querier(t, testdb.RoleClean, database))
	if err != nil {
		t.Fatal(err)
	}
	wantFindings(t, report, critical("function_execute",
		"EXECUTE on 2 functions: public.autoprewarm_dump_now, public.autoprewarm_start_worker"))
	if !report.Blocking() {
		t.Error("Blocking() = false: role_check strict would admit this role")
	}
}
