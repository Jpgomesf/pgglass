package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// writableTables is a fixture in which ro_clean may write the twelve
// long-named tables of a schema: its role check reports a table_write
// warning that names ten of them, about 1.2 kB. It grants to a fixture role
// rather than create one, since TestBypassCorpusIsRejectedAndChangesNothing
// checks that the cluster's roles do not change while it runs.
func writableTables() string {
	var fixture strings.Builder
	fixture.WriteString("CREATE SCHEMA analytics_reporting_warehouse;\n")
	for i := range 12 {
		fmt.Fprintf(&fixture, "CREATE TABLE analytics_reporting_warehouse.customer_lifetime_value_snapshot_by_region_%02d (id int);\n", i)
	}
	fixture.WriteString(`GRANT USAGE ON SCHEMA analytics_reporting_warehouse TO ro_clean;
	GRANT ALL ON ALL TABLES IN SCHEMA analytics_reporting_warehouse TO ro_clean;`)
	return fixture.String()
}

// pg_list_connections returns every connection with its role check report,
// with no bound: ten connections whose role may write the tables it reads,
// each reported as a table_write warning that names ten of them, took 12,447
// bytes.
func TestListConnectionsStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_list_inline_bound"
	testdb.CreateDatabase(t, name, writableTables())
	conns := map[string]config.Connection{}
	for i := range 10 {
		conns[fmt.Sprintf("warehouse-%d", i)] = direct(testdb.DSN(t, testdb.RoleClean, name))
	}
	s := connect(t, newConfig(config.RoleCheckWarn, conns))
	for conn := range conns {
		// The first use audits the role, and the result starts with its report.
		got := s.ok(t, "pg_query", args(conn, "SELECT 1"))
		if !strings.HasPrefix(got, "role check: role ro_clean: 1 finding(s)\n  warning table_write: 12 relations, first 10: ") {
			t.Fatalf("%s: first call:\n%.300s", conn, got)
		}
	}

	if got := s.ok(t, "pg_list_connections", map[string]any{}); len(got) > config.InlineMaxBytes {
		t.Errorf("pg_list_connections returned %d bytes, limit %d", len(got), config.InlineMaxBytes)
	}
}
