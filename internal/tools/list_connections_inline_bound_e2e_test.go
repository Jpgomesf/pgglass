package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

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

// pg_list_connections stays within the inline limit when every connection's
// role check report is long.
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
		got := s.ok(t, "pg_query", args(conn, "SELECT 1"))
		if !strings.HasPrefix(got, "role check: role ro_clean: 1 finding(s)\n  warning table_write: 12 relations, first 10: ") {
			t.Fatalf("%s: first call:\n%.300s", conn, got)
		}
	}

	if got := s.ok(t, "pg_list_connections", map[string]any{}); len(got) > config.InlineMaxBytes {
		t.Errorf("pg_list_connections returned %d bytes, limit %d", len(got), config.InlineMaxBytes)
	}
}
