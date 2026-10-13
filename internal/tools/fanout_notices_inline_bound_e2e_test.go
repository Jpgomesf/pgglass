package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Under role_check warn, a fan-out's first call starts with the report of
// each connection whose role has findings: ten reports of a role that may
// write twelve long-named tables, each a table_write warning naming ten of
// them, took 12 kB before any row. Now they take at most half the inline
// limit, then point to pg_list_connections, whose spill file holds every
// report whole, and the rows follow.
func TestFanoutRoleCheckNoticesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_fanout_notices_bound"
	testdb.CreateDatabase(t, name, writableTables())
	conns := map[string]config.Connection{}
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("warehouse-%d", i))
		conns[names[i]] = direct(testdb.DSN(t, testdb.RoleClean, name))
	}
	s := connect(t, newConfig(config.RoleCheckWarn, conns))

	got := s.ok(t, "pg_query", fanoutArgs(names, "SELECT repeat('x', 100) AS v FROM generate_series(1, 4)"))
	notice, rows, cut := strings.Cut(got, "… more role check findings: pg_list_connections shows them all\n\n")
	if len(got) > config.InlineMaxBytes || !cut || len(notice) > config.InlineMaxBytes/2 ||
		!strings.HasPrefix(notice, "warehouse-0: role check: role ro_clean: 1 finding(s)\n  warning table_write: 12 relations, first 10: ") ||
		!strings.HasPrefix(rows, "connection\tv\nwarehouse-0\txxx") || !strings.HasSuffix(rows, "\n(40 rows)") {
		t.Fatalf("first fan-out call, %d bytes:\n%s", len(got), got)
	}

	list := s.ok(t, "pg_list_connections", nil)
	if len(list) > config.InlineMaxBytes {
		t.Fatalf("pg_list_connections returned %d bytes", len(list))
	}
	records := readSpill(t, spillPath(t, list), s.spillDir)
	if reports, whole := countReports(records); records[1][0] != "Connections (10):" || reports != 10 || whole != 10 {
		t.Errorf("spill file of pg_list_connections: %d reports, %d whole findings, want 10 of each:\n%q", reports, whole, records)
	}
}

// countReports counts, in the spill file of pg_list_connections, the
// reports of ro_clean and their table_write findings, whole.
func countReports(records [][]string) (reports, whole int) {
	for _, r := range records {
		switch {
		case r[0] == "    role ro_clean: 1 finding(s)":
			reports++
		case strings.HasPrefix(r[0], "      warning table_write: 12 relations, first 10: ") && strings.HasSuffix(r[0], "_by_region_09"):
			whole++
		}
	}
	return reports, whole
}
