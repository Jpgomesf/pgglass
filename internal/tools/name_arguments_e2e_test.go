package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

// Postgres keeps names of at most 63 bytes and cuts a longer one to its
// first 63, which may name another object: a tool given one read what those
// bytes name and repeated the whole name in its output. Every argument that
// names a schema, table or column now refuses a longer name, and so does a
// default_schema that takes its place, before the database: the DSN is never
// dialed.
func TestNameArgumentsOver63BytesAreRefused(t *testing.T) {
	long := strings.Repeat("n", 64)
	s := connect(t, newConfig(config.RoleCheckStrict, map[string]config.Connection{
		"db":   direct(unusedDSN),
		"app":  {Source: "direct", DSN: unusedDSN, DefaultSchema: "app"},
		"long": {Source: "direct", DSN: unusedDSN, DefaultSchema: long},
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
		{"pg_schema", map[string]any{"connection": "long", "table": "t"}, refused("schema")},
		{"pg_query", args("long", "SELECT 1"), refused("schema")},
		{"pg_explain", args("long", "SELECT 1"), refused("schema")},
		{"pg_profile", map[string]any{"connection": "long", "table": "t"}, refused("schema")},
	} {
		if got := s.fails(t, tc.tool, tc.args); got != tc.want {
			t.Errorf("%s %v:\n%s\nwant\n%s", tc.tool, tc.args, got, tc.want)
		}
	}
}
