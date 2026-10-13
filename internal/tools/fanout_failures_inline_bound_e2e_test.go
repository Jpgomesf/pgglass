package tools_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// A fanned-out pg_query that fails on some connections lists their errors
// after the rows. fanoutText appended those lines after the rows had been
// fitted to InlineMaxBytes, so they came on top of the limit: 9,997 bytes of
// rows from app plus other's 88-byte "relation does not exist" line took
// 10,085 bytes, and an error that quotes a 20,000-character value took
// 20,113.
func TestFanoutFailureLinesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"app":   direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)),
		"other": direct(testdb.DSN(t, testdb.RoleSupabase, testdb.DBSupabase)), // has no app.customers
	}))

	for _, sql := range []string{
		// On app alone, 95 rows of 100 characters take 9,997 bytes.
		"SELECT repeat('x', 100) AS v FROM generate_series(1, 95) WHERE (SELECT count(*) FROM app.customers) > 0",
		// On other, the cast fails quoting its 20,000-character input.
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
