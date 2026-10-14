package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Under role_check strict, an audit Postgres refuses fails the call with an
// error saying the role check failed, and the audit line keeps the SQLSTATE.
func TestRoleCheckStrictSaysWhenPostgresRefusedTheAudit(t *testing.T) {
	t.Parallel()
	const name = "tools_rolecheck_noproc"
	testdb.CreateDatabase(t, name, "REVOKE SELECT ON pg_catalog.pg_proc FROM PUBLIC")
	conns := appConns(t)
	conns["noproc"] = direct(testdb.DSN(t, testdb.RoleClean, name))
	s := connect(t, newConfig(config.RoleCheckStrict, conns))

	const failed = "role check failed, and role_check strict refuses connections it cannot check: " +
		"ERROR: permission denied for table pg_proc (SQLSTATE 42501)"
	if got := s.fails(t, "pg_query", args("noproc", "SELECT 1")); got != failed {
		t.Errorf("pg_query:\n%s\nwant\n%s", got, failed)
	}
	if got := s.ok(t, "pg_query", fanoutArgs([]string{"clean", "noproc"}, "SELECT 1 AS one")); !strings.HasSuffix(got, "\nnoproc: "+failed) {
		t.Errorf("fan-out:\n%s\nwant it to end with\nnoproc: %s", got, failed)
	}
	for _, a := range []map[string]any{diffArgs("clean", "noproc"), diffArgs("noproc", "clean")} {
		if got := s.fails(t, "pg_diff_schema", a); got != "noproc: "+failed {
			t.Errorf("pg_diff_schema %v:\n%s\nwant\nnoproc: %s", a, got, failed)
		}
	}
	for _, line := range s.auditLines(t) {
		if line.Connection != "clean" && (line.Outcome != "db_error" || line.SQLState != "42501") {
			t.Errorf("audit line %+v, want a db_error with SQLSTATE 42501", line)
		}
	}
}
