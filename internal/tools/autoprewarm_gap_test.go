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

// autoprewarmFile is the size of $PGDATA/autoprewarm.blocks, -1 while there is
// none.
const autoprewarmFile = `SELECT coalesce((pg_stat_file('autoprewarm.blocks', true)).size, -1)`

// A plain read-only role must not be able to make the server write a server
// file. autoprewarm_dump_now() wrote $PGDATA/autoprewarm.blocks, past READ
// ONLY and the rollback: the guard allowed it, and the role check did not
// flag it. Now the guard rejects it before it reaches the database.
func TestGapAutoprewarmWritesServerFile(t *testing.T) {
	t.Parallel()
	const name = "secverify_autoprewarm_reg"
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"ro": prewarmDB(t, name)}))
	admin := testdb.Conn(t, testdb.Superuser, name)
	before := testdb.Value[int64](t, admin, autoprewarmFile)

	got := s.fails(t, "pg_query", args("ro", `SELECT autoprewarm_dump_now()`))
	if want := "query rejected: function autoprewarm_dump_now is not allowed"; got != want {
		t.Errorf("pg_query = %q, want %q", got, want)
	}
	if after := testdb.Value[int64](t, admin, autoprewarmFile); after != before {
		t.Errorf("a read-only role made the server write autoprewarm.blocks (size %d -> %d)", before, after)
	}
}

// The role check flags EXECUTE on autoprewarm_* as critical, so role_check
// strict refuses ro_clean where pg_prewarm is installed, and certified it as
// clean before.
func TestGapAutoprewarmRoleCheck(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, map[string]config.Connection{
		"ro": prewarmDB(t, "secverify_autoprewarm_strict"),
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
