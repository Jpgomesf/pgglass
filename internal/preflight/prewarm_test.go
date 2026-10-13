package preflight_test

import (
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/preflight"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// CREATE EXTENSION pg_prewarm leaves autoprewarm_dump_now, which writes
// $PGDATA/autoprewarm.blocks, and autoprewarm_start_worker, which starts a
// worker that rewrites it, executable by PUBLIC. So ro_clean, which has
// nothing else to report, gets a critical finding there, and role_check
// strict refuses it.
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
