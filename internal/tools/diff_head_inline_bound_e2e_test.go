package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// A schema argument over 63 bytes is refused without being repeated, so
// pg_diff_schema's result stays within the inline limit.
func TestDiffSchemaHeadStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	schema := strings.Repeat("s", 63)
	testdb.CreateDatabase(t, "tools_diff_head_left", "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".a (id int);")
	testdb.CreateDatabase(t, "tools_diff_head_right", "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".b (id int);")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"left":  direct(testdb.DSN(t, testdb.Superuser, "tools_diff_head_left")),
		"right": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_head_right")),
	}))

	got := s.fails(t, "pg_diff_schema", diffArgs("left", "right", "schema", schema+strings.Repeat("x", 12000)))
	if want := "schema is 12063 bytes: Postgres names take at most 63, and it cuts a longer one"; got != want {
		t.Errorf("a 12,063-byte schema = %.200q, want %q", got, want)
	}
}
