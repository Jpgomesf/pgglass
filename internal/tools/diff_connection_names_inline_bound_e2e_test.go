package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_diff_schema's first line names both connections, twice when they
// differ, and comes on top of a summary fitted to what it leaves, or alone
// and unchecked when they match. Connection names of 5,100 bytes took about
// 20.7 kB and 10.2 kB.
//
// Config now refuses connection names over 63 bytes, and pg_diff_schema a
// schema over 63, so the line takes at most 387 bytes: four names, the
// schema, a count of 19 digits at most and 53 bytes of words. That is well
// within the 5,000 the role check's findings leave at least, and a summary of
// the differences fits the rest.
func TestDiffSchemaLongConnectionNamesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	if err := parseConnectionNames(strings.Repeat("l", 5100), strings.Repeat("r", 5100)); err == nil ||
		!strings.Contains(err.Error(), " is 5100 bytes: connection names take at most 63") {
		t.Errorf("config.json with names of 5,100 bytes: %v", err)
	}
	// 150 tables of 63-byte names only on the right: 10.6 kB of differences.
	// Each name starts with its number: a table's array type takes the first
	// 62 bytes of its name after an underscore, and PostgreSQL 15 finds no
	// free name for it when too many tables share those bytes.
	testdb.CreateDatabase(t, "tools_diff_long_names_left", "CREATE TABLE a (id int);")
	testdb.CreateDatabase(t, "tools_diff_long_names_right", `CREATE TABLE a (id int);
		DO $$ BEGIN FOR i IN 1..150 LOOP
			EXECUTE format('CREATE TABLE %I (id int)', 'b' || lpad(i::text, 3, '0') || repeat('b', 59));
		END LOOP; END $$;`)
	left := direct(testdb.DSN(t, testdb.Superuser, "tools_diff_long_names_left"))
	right := direct(testdb.DSN(t, testdb.Superuser, "tools_diff_long_names_right"))
	l, r, twin := strings.Repeat("l", 63), strings.Repeat("r", 63), strings.Repeat("t", 63)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{l: left, r: right, twin: left}))

	got := s.ok(t, "pg_diff_schema", diffArgs(l, r))
	head, summary, _ := strings.Cut(got, "\n")
	if want := "schema public, " + l + " → " + r + ": 150 differences (- only in " + l + ", + only in " + r + ")"; head != want {
		t.Errorf("first line:\n%s\nwant\n%s", head, want)
	}
	if len(got) > config.InlineMaxBytes || !strings.HasPrefix(summary, "Result too large to return inline (") {
		t.Errorf("differing: %d bytes, limit %d:\n%s", len(got), config.InlineMaxBytes, got)
	}
	if got, want := s.ok(t, "pg_diff_schema", diffArgs(l, twin)), "schema public, "+l+" → "+twin+": identical (1 relation)"; got != want {
		t.Errorf("identical:\n%s\nwant\n%s", got, want)
	}
}
