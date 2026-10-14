package tools_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_profile's cells come back whole, such as a common cell of five uuids and
// their shares, 218 characters.
func TestProfileShowsLongCommonValuesWhole(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_profile_uuids", `CREATE TABLE tokens (id uuid);
		INSERT INTO tokens SELECT ('00000000-0000-0000-0000-00000000000' || n)::uuid
			FROM generate_series(1, 5) AS n, generate_series(1, 35 - 5 * n);
		ANALYZE tokens;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, "tools_profile_uuids")),
	}))

	want := "public.tokens: ~100 rows, from pg_stats\n" +
		"column\ttype\tnulls\tdistinct\twidth\tcommon\tmin\tmax\n" +
		"id\tuuid\t0%\t5\t16\t" +
		"'00000000-0000-0000-0000-000000000001' 30%, '00000000-0000-0000-0000-000000000002' 25%, " +
		"'00000000-0000-0000-0000-000000000003' 20%, '00000000-0000-0000-0000-000000000004' 15%, " +
		"'00000000-0000-0000-0000-000000000005' 10%\t\t\n" +
		"(1 row)"
	if got := s.ok(t, "pg_profile", map[string]any{"connection": "db", "table": "public.tokens"}); got != want {
		t.Errorf("pg_profile:\n%s\nwant\n%s", got, want)
	}
}
