package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_profile names the table as the call spells it, but Postgres cuts a name
// parameter to 63 bytes, so a table argument of 12,070 bytes still matched
// the 63-byte table: the line naming it took more than the inline limit, the
// rows spilled, and the result took about 12.3 kB. A schema, table or column
// over 63 bytes is now refused before any query. Under that cap the line
// takes at most about 350 bytes (two names of 63, the note on a sample or on
// pg_stats, a count of the columns not shown), and the rows fit what it
// leaves of the 5,000 the role check's findings leave at least.
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
