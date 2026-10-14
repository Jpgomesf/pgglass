package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

// Every schema, table or column argument over 63 bytes, which Postgres would
// cut, is refused before the DSN is dialed.
func TestNameArgumentsOver63BytesAreRefused(t *testing.T) {
	long := strings.Repeat("n", 64)
	s := connect(t, newConfig(config.RoleCheckStrict, map[string]config.Connection{
		"db":  direct(unusedDSN),
		"app": {Source: "direct", DSN: unusedDSN, DefaultSchema: "app"},
	}))
	refused := func(arg string) string {
		return arg + " is 64 bytes: Postgres names take at most 63, and it cuts a longer one"
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pg_schema", map[string]any{"connection": "db", "schema": long}, refused("schema")},
		{"pg_schema", map[string]any{"connection": "db", "schema": "app", "table": long}, refused("table")},
		{"pg_schema", map[string]any{"connection": "app", "table": long}, refused("table")},
		{"pg_query", args("db", "SELECT 1", "schema", long), refused("schema")},
		{"pg_query", fanoutArgs([]string{"db", "app"}, "SELECT 1", "schema", long), "db: " + refused("schema") + "\napp: " + refused("schema")},
		{"pg_explain", args("db", "SELECT 1", "schema", long), refused("schema")},
		{"pg_profile", map[string]any{"connection": "db", "table": "app." + long}, refused("table")},
		{"pg_profile", map[string]any{"connection": "app", "table": long}, refused("table")},
		{"pg_profile", map[string]any{"connection": "db", "table": long + ".t"}, refused("schema")},
		{"pg_profile", map[string]any{"connection": "db", "schema": long, "table": "t"}, refused("schema")},
		{"pg_profile", map[string]any{"connection": "db", "table": "app.t", "column": long}, refused("column")},
		{"pg_diff_schema", diffArgs("db", "app", "schema", long), refused("schema")},
	} {
		if got := s.fails(t, tc.tool, tc.args); got != tc.want {
			t.Errorf("%s %v:\n%s\nwant\n%s", tc.tool, tc.args, got, tc.want)
		}
	}
}
