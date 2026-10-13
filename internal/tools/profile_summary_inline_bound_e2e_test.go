package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_profile fits its rows to InlineMaxBytes, then puts the line that names
// the table before them, so rows that just fit came back over the limit: two
// columns of five most common values of 976 characters render in 9,946
// bytes, and the line before them, with its line break, took 94 more.
func TestProfileSummaryLineStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_profile_inline_bound"
	table := strings.Repeat("p", 60)
	testdb.CreateDatabase(t, name, `CREATE TABLE `+table+` (a text, b text);
		INSERT INTO `+table+` SELECT repeat(chr(64 + k), 976), repeat(chr(96 + k), 976)
			FROM generate_series(1, 5) AS k, generate_series(1, 10 * k);
		ANALYZE `+table+`;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	got := s.ok(t, "pg_profile", map[string]any{"connection": "db", "table": "public." + table})
	if !strings.HasPrefix(got, "public."+table+": ~150 rows, from pg_stats\n") {
		t.Fatalf("pg_profile:\n%.300s", got)
	}
	if len(got) > config.InlineMaxBytes {
		t.Errorf("pg_profile returned %d bytes, limit %d", len(got), config.InlineMaxBytes)
	}
}
