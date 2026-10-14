package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_diff_schema's first line names both connections; with names capped at 63
// bytes it takes at most 387, and the result stays within the inline limit.
func TestDiffSchemaLongConnectionNamesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	if err := parseConnectionNames(strings.Repeat("l", 5100), strings.Repeat("r", 5100)); err == nil ||
		!strings.Contains(err.Error(), " is 5100 bytes: connection names take at most 63") {
		t.Errorf("config.json with names of 5,100 bytes: %v", err)
	}
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
