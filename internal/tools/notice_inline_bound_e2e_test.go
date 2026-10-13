package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// Under role_check warn, the first call on a connection whose role has
// findings starts with the role check's report. handle prepends that report
// after the tool has fitted its output to InlineMaxBytes, so an output that
// just fit came back over the limit: 9,808 bytes of pg_query rows plus
// ro_leaky's 472-byte report, and a pg_schema listing cut to 9,990 bytes plus
// the same report.
func TestRoleCheckNoticeStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_notice_inline_bound"
	testdb.CreateDatabase(t, name, testdb.AppSQL()+`
		DO $$ BEGIN FOR i IN 1..300 LOOP
			EXECUTE format('CREATE TABLE app.%s_%s (id int)', repeat('r', 55), lpad(i::text, 4, '0'));
		END LOOP; END $$;
		GRANT SELECT ON ALL TABLES IN SCHEMA app TO ro_leaky;`)
	dsn := testdb.DSN(t, testdb.RoleLeaky, name)
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"for_query":  direct(dsn),
		"for_schema": direct(dsn),
	}))

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"pg_query", args("for_query", "SELECT repeat('x', 100) AS v FROM generate_series(1, 97)")},
		{"pg_schema", map[string]any{"connection": "for_schema", "schema": "app"}},
	} {
		got := s.ok(t, tc.tool, tc.args) // the connection's first call
		if !strings.HasPrefix(got, "role check: role ro_leaky: ") {
			t.Fatalf("%s: the first call does not start with the role check's findings:\n%.300s", tc.tool, got)
		}
		if len(got) > config.InlineMaxBytes {
			t.Errorf("%s: the first call returned %d bytes, limit %d", tc.tool, len(got), config.InlineMaxBytes)
		}
	}
}
