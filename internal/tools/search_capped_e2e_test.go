package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// max_rows caps pg_search, but a capped search did not say so: 1,200
// columns match, the default max_rows of 1000 keeps the first 1000, and the
// spill summary reported "1000 rows × 1 column, truncated=false", so the
// agent took the file for every match. Now it says "truncated=true
// (max_rows=1000)", as pg_query does for the same cut, or names the limit
// the agent asked for when that is lower.
func TestSearchCappedByMaxRowsSaysSo(t *testing.T) {
	t.Parallel()
	const name = "tools_search_capped"
	cols := make([]string, 1200)
	for i := range cols {
		cols[i] = fmt.Sprintf("col_%04d_z text", i)
	}
	testdb.CreateDatabase(t, name, "CREATE TABLE wide ("+strings.Join(cols, ", ")+")")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	for _, tc := range []struct {
		limit int
		want  string
	}{
		{5000, ": 1000 rows × 1 column, truncated=true (max_rows=1000).\n"},
		{950, ": 950 rows × 1 column, truncated=true (limit=950).\n"},
	} {
		got := s.ok(t, "pg_search", map[string]any{"connection": "db", "pattern": "col_", "limit": tc.limit})
		if len(got) > config.InlineMaxBytes || !strings.Contains(got, tc.want) {
			t.Errorf("limit %d: pg_search returned %d bytes:\n%.400s", tc.limit, len(got), got)
		}
		records := readSpill(t, spillPath(t, got), s.spillDir)
		if want := min(tc.limit, 1000); len(records) != want+1 || records[want][0] != fmt.Sprintf("public.wide.col_%04d_z: text", want-1) {
			t.Errorf("limit %d: %d CSV records, want a header and the first %d matches", tc.limit, len(records), want)
		}
	}
	lines := s.auditLines(t)
	if len(lines) != 2 || !lines[0].Truncated || lines[0].Rows != 1000 || !lines[1].Truncated || lines[1].Rows != 950 {
		t.Errorf("audit lines = %+v, want 1000 and 950 rows, both truncated", lines)
	}
}
