package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_diff_schema, like pg_explain and pg_search, writes text it finds too
// large to return inline to a spill file, one line per row. The spill store
// cut each line to 200 characters and, since that fit, returned the lines
// inline without writing a file: here 30 '~ index' lines of 350 characters,
// each with two CREATE INDEX statements, lost their right-hand side. Now
// they always go to the file, whole.
func TestDiffSchemaSpillsLongLinesWhole(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 60), strings.Repeat("b", 60)
	fixture := func(columns string) string {
		return "CREATE TABLE t (" + a + " int, " + b + " int);\n" +
			"DO $$ BEGIN FOR i IN 1..30 LOOP\n" +
			"	EXECUTE format('CREATE INDEX ix_%s ON t (" + columns + ")', lpad(i::text, 2, '0'));\n" +
			"END LOOP; END $$;"
	}
	testdb.CreateDatabase(t, "tools_diff_long_left", fixture(a+", "+b))
	testdb.CreateDatabase(t, "tools_diff_long_right", fixture(b+", "+a))
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"left":  direct(testdb.DSN(t, testdb.Superuser, "tools_diff_long_left")),
		"right": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_long_right")),
	}))

	got := s.ok(t, "pg_diff_schema", diffArgs("left", "right"))
	head, summary, _ := strings.Cut(got, "\n")
	if want := "schema public, left → right: 30 differences (- only in left, + only in right)"; head != want {
		t.Errorf("first line = %q, want %q", head, want)
	}
	if len(got) > config.InlineMaxBytes || !strings.HasPrefix(summary, "Result too large to return inline (") ||
		!strings.Contains(summary, ": 30 rows × 1 column, truncated=false.\n") {
		t.Fatalf("pg_diff_schema returned %d bytes:\n%s", len(got), got)
	}
	records := readSpill(t, spillPath(t, summary), s.spillDir)
	if len(records) != 31 || records[0][0] != "difference" {
		t.Fatalf("%d CSV records, want a header and 30 differences", len(records))
	}
	def := func(i int, columns string) string {
		return fmt.Sprintf("CREATE INDEX ix_%02d ON public.t USING btree (%s)", i, columns)
	}
	for i := 1; i <= 30; i++ {
		if want := fmt.Sprintf("~ index t.ix_%02d: %s → %s", i, def(i, a+", "+b), def(i, b+", "+a)); records[i][0] != want {
			t.Errorf("record %d = %q, want %q", i, records[i][0], want)
		}
	}
}
