package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// A Postgres error can quote a value whole, and the error result returned it
// whole: casting a 20,000-character text value to int returned a 20 kB error
// from pg_query, from pg_explain with analyze, and from a fan-out on which
// every connection failed.
func TestErrorTextStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_error_inline_bound"
	testdb.CreateDatabase(t, name, `CREATE TABLE notes (body text);
		INSERT INTO notes VALUES (repeat('y', 20000));`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	const sql = "SELECT body::int FROM notes"
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"pg_query", args("db", sql)},
		{"pg_explain", args("db", sql, "analyze", true)},
		{"pg_query", fanoutArgs([]string{"db"}, sql)},
	} {
		got := s.fails(t, tc.tool, tc.args)
		if !strings.Contains(got, "ERROR: invalid input syntax for type integer") {
			t.Fatalf("%s %v:\n%.300s", tc.tool, tc.args, got)
		}
		if len(got) > config.InlineMaxBytes {
			t.Errorf("%s %v: returned %d bytes, limit %d", tc.tool, tc.args, len(got), config.InlineMaxBytes)
		}
	}
}
