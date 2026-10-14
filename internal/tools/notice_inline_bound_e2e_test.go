package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Under role_check warn, the role check's report and the output it precedes fit
// within InlineMaxBytes together.
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
		got := s.ok(t, tc.tool, tc.args)
		if !strings.HasPrefix(got, "role check: role ro_leaky: ") {
			t.Fatalf("%s: the first call does not start with the role check's findings:\n%.300s", tc.tool, got)
		}
		if len(got) > config.InlineMaxBytes {
			t.Errorf("%s: the first call returned %d bytes, limit %d", tc.tool, len(got), config.InlineMaxBytes)
		}
	}
}
