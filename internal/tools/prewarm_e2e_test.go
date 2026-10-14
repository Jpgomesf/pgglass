package tools_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func prewarmDB(t *testing.T, name string) config.Connection {
	t.Helper()
	testdb.CreateDatabase(t, name, "CREATE EXTENSION pg_prewarm; CREATE TABLE t (v int); GRANT SELECT ON t TO "+testdb.RoleClean)
	return direct(testdb.DSN(t, testdb.RoleClean, name))
}

// pg_prewarm's functions act past READ ONLY and the rollback, so pg_query
// rejects them before the database, though role_check warn admits ro_clean.
func TestQueryRejectsPrewarmFunctions(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"ro": prewarmDB(t, "tools_prewarm_query"),
	}))
	for _, tc := range []struct{ sql, function string }{
		{`SELECT autoprewarm_dump_now()`, "autoprewarm_dump_now"},
		{`SELECT pg_prewarm('t')`, "pg_prewarm"},
	} {
		got := s.fails(t, "pg_query", args("ro", tc.sql))
		if want := "query rejected: function " + tc.function + " is not allowed"; got != want {
			t.Errorf("pg_query %s = %q, want %q", tc.sql, got, want)
		}
	}
}

// The role check flags EXECUTE on autoprewarm_* as critical, so role_check
// strict refuses ro_clean where pg_prewarm is installed.
func TestRoleCheckStrictRefusesAutoprewarm(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, map[string]config.Connection{
		"ro": prewarmDB(t, "tools_autoprewarm_strict"),
	}))
	want := "connection \"ro\" refused (role_check strict): its role can do more than read.\n" +
		"role ro_clean: 1 finding(s)\n" +
		"  critical function_execute: EXECUTE on 2 functions: public.autoprewarm_dump_now, public.autoprewarm_start_worker"
	if got := s.fails(t, "pg_query", args("ro", "SELECT 1")); got != want {
		t.Errorf("pg_query under role_check strict:\n%s\nwant\n%s", got, want)
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Outcome != "preflight_block" {
		t.Errorf("audit lines = %+v, want one preflight_block", lines)
	}
}
