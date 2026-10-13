package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_search output stays within InlineMaxBytes, as every tool's does: the
// 900 matches below, 25 KB, are written to a spill file, one per row, and
// summarized.
func TestSearchSpillsMatchesPastTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_search_spill"
	cols := make([]string, 900)
	for i := range cols {
		cols[i] = fmt.Sprintf("col_%03d_z text", i)
	}
	testdb.CreateDatabase(t, name, "CREATE TABLE wide ("+strings.Join(cols, ", ")+")")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	got := s.ok(t, "pg_search", map[string]any{"connection": "db", "pattern": "col_", "limit": 900})
	if len(got) > config.InlineMaxBytes || !strings.HasPrefix(got, "Result too large to return inline (") ||
		!strings.Contains(got, ": 900 rows × 1 column, truncated=false.\n") {
		t.Fatalf("pg_search returned %d bytes, limit %d:\n%s", len(got), config.InlineMaxBytes, got)
	}
	records := readSpill(t, spillPath(t, got), s.spillDir)
	if len(records) != 901 {
		t.Fatalf("%d CSV records, want a header and 900 matches", len(records))
	}
	for i, want := range map[int]string{0: "match", 1: "public.wide.col_000_z: text", 900: "public.wide.col_899_z: text"} {
		if got := strings.Join(records[i], ","); got != want {
			t.Errorf("record %d = %q, want %q", i, got, want)
		}
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Rows != 900 || lines[0].Outcome != "ok" {
		t.Errorf("audit lines = %+v, want one ok with 900 rows", lines)
	}

	// Within the limit, the matches come inline, one per line.
	if got := s.ok(t, "pg_search", map[string]any{"connection": "db", "pattern": "col_899"}); got != "public.wide.col_899_z: text" {
		t.Errorf("pg_search col_899 = %q", got)
	}
}
