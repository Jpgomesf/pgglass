package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_profile refuses names over 63 bytes before any query, so the line naming
// the table and its rows stay within the inline limit.
func TestProfileLongTableArgumentStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_profile_long_table_argument"
	table := strings.Repeat("p", 63)
	testdb.CreateDatabase(t, name, "CREATE TABLE "+table+" (id int, note text);\n"+
		"INSERT INTO "+table+" SELECT g, 'n' || (g % 3) FROM generate_series(1, 100) AS g;\n"+
		"ANALYZE "+table+";")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	got := s.ok(t, "pg_profile", map[string]any{"connection": "db", "table": "public." + table})
	if !strings.HasPrefix(got, "public."+table+": ~100 rows, from pg_stats\n") {
		t.Errorf("a 63-byte table:\n%s", got)
	}
	junk := strings.Repeat("x", 12000)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"table": "public." + table + junk}, "table is 12063 bytes"},
		{map[string]any{"table": table + junk, "schema": "public"}, "table is 12063 bytes"},
		{map[string]any{"table": table, "schema": "public" + junk}, "schema is 12006 bytes"},
		{map[string]any{"table": "public" + junk + "." + table}, "schema is 12006 bytes"},
		{map[string]any{"table": "public." + table, "column": "note" + junk}, "column is 12004 bytes"},
	} {
		tc.args["connection"] = "db"
		want := tc.want + ": Postgres names take at most 63, and it cuts a longer one"
		if got := s.fails(t, "pg_profile", tc.args); got != want {
			t.Errorf("got %d bytes:\n%.300s\nwant\n%s", len(got), got, want)
		}
	}
}
