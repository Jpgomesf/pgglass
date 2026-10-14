package tools_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// A fanned-out pg_query that fails on some connections fits their error lines
// and its rows together within InlineMaxBytes.
func TestFanoutFailureLinesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"app":   direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)),
		"other": direct(testdb.DSN(t, testdb.RoleSupabase, testdb.DBSupabase)),
	}))

	for _, sql := range []string{
		"SELECT repeat('x', 100) AS v FROM generate_series(1, 95) WHERE (SELECT count(*) FROM app.customers) > 0",
		"SELECT CASE WHEN current_database() = 'app' THEN 1 ELSE (repeat('y', 20000) || current_database())::int END AS v",
	} {
		before := len(s.auditLines(t))
		got := s.ok(t, "pg_query", fanoutArgs([]string{"app", "other"}, sql))
		failed := 0
		for _, line := range s.auditLines(t)[before:] {
			if line.Connection == "other" && line.Outcome == "db_error" {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("%s: other did not fail:\n%.300s", sql, got)
		}
		if len(got) > config.InlineMaxBytes {
			t.Errorf("%.40s...: returned %d bytes, limit %d", sql, len(got), config.InlineMaxBytes)
		}
	}
}
