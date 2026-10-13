package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_diff_schema puts its first line, which names the schema as given,
// before a summary already fitted to InlineMaxBytes. Postgres cuts a name
// parameter to 63 bytes, so a 12,063-byte schema argument still matched the
// 63-byte schema, and the result took about 12.6 kB.
func TestDiffSchemaHeadStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	schema := strings.Repeat("s", 63)
	testdb.CreateDatabase(t, "tools_diff_head_left", "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".a (id int);")
	testdb.CreateDatabase(t, "tools_diff_head_right", "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".b (id int);")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"left":  direct(testdb.DSN(t, testdb.Superuser, "tools_diff_head_left")),
		"right": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_head_right")),
	}))

	got, _ := s.call(t, "pg_diff_schema", diffArgs("left", "right", "schema", schema+strings.Repeat("x", 12000)))
	if len(got) > config.InlineMaxBytes {
		t.Errorf("pg_diff_schema returned %d bytes, limit %d:\n%.200s", len(got), config.InlineMaxBytes, got)
	}
}
