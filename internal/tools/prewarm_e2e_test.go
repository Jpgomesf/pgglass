package tools_test

import (
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// prewarmDB creates database name with pg_prewarm, whose autoprewarm_*
// functions CREATE EXTENSION leaves executable by PUBLIC, and returns a
// connection to it as ro_clean, a role with nothing else to report.
func prewarmDB(t *testing.T, name string) config.Connection {
	t.Helper()
	testdb.CreateDatabase(t, name, "CREATE EXTENSION pg_prewarm")
	return direct(testdb.DSN(t, testdb.RoleClean, name))
}

// autoprewarm_dump_now writes $PGDATA/autoprewarm.blocks, past READ ONLY and
// the rollback. role_check warn admits ro_clean, which may execute it, so the
// guard is what stops it: pg_query rejects the call before it reaches the
// database.
func TestQueryRejectsAutoprewarm(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"ro": prewarmDB(t, "tools_autoprewarm_query"),
	}))
	got := s.fails(t, "pg_query", args("ro", `SELECT autoprewarm_dump_now()`))
	if want := "query rejected: function autoprewarm_dump_now is not allowed"; got != want {
		t.Errorf("pg_query = %q, want %q", got, want)
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
