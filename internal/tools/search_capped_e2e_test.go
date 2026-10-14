package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// A pg_search cut by max_rows says truncated=true (max_rows=1000) in its spill
// summary, or names the lower limit the agent asked for.
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

// An inline search cut by limit or max_rows ends with a line naming the limit.
func TestSearchCutInlineSaysSo(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, appConns(t))
	cfg.Defaults.MaxRows = 2
	s := connect(t, cfg)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"connection": "clean", "pattern": "order", "limit": 1}, "app.customer_order_totals: view\n(truncated: limit=1)"},
		{map[string]any{"connection": "clean", "pattern": "order"}, "app.customer_order_totals: view\napp.orders: table\n(truncated: max_rows=2)"},
		{map[string]any{"connection": "clean", "pattern": "tenant_notes"}, "app.tenant_notes: table"},
	} {
		if got := s.ok(t, "pg_search", tc.args); got != tc.want {
			t.Errorf("pg_search %v:\n%s\nwant\n%s", tc.args, got, tc.want)
		}
	}
	lines := s.auditLines(t)
	if len(lines) != 3 || !lines[0].Truncated || !lines[1].Truncated || lines[2].Truncated {
		t.Errorf("audit lines = %+v, want the first two truncated", lines)
	}
}
