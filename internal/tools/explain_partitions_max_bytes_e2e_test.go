package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// EXPLAIN (FORMAT JSON, VERBOSE) of a scan of a partitioned table lists every
// partition with its output columns: 1200 partitions of 21 columns make a plan
// of 1.26 MB, over the default max_bytes, and more with ANALYZE. pg_explain
// summarizes it, so the plan's size must not fail the call; with ANALYZE, each
// empty partition is flagged for returning fewer rows than planned, and that
// summary goes to a spill file.
func TestExplainSummarizesAPlanOverMaxBytes(t *testing.T) {
	t.Parallel()
	const db = "tools_explain_plan_max_bytes"
	cols := make([]string, 20)
	for i := range cols {
		cols[i] = fmt.Sprintf("column_number_%02d text", i)
	}
	testdb.CreateDatabase(t, db, "CREATE TABLE events (id int, "+strings.Join(cols, ", ")+") PARTITION BY RANGE (id);\n"+
		`DO $$ BEGIN FOR i IN 0..1199 LOOP
			EXECUTE format('CREATE TABLE events_%s PARTITION OF events FOR VALUES FROM (%s) TO (%s)', i, i * 10, i * 10 + 10);
		END LOOP; END $$;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, db))}))

	for _, analyze := range []bool{false, true} {
		got, isErr := s.call(t, "pg_explain", map[string]any{"connection": "db", "sql": "SELECT * FROM events", "analyze": analyze})
		summary := got
		if !isErr && analyze {
			var lines []string
			for _, record := range readSpill(t, spillPath(t, got), s.spillDir)[1:] {
				lines = append(lines, record[0])
			}
			summary = strings.Join(lines, "\n")
		}
		if isErr || !strings.HasPrefix(summary, "total cost ") || !strings.Contains(summary, "\n  Seq Scan on public.events_") || len(got) > config.InlineMaxBytes {
			t.Errorf("pg_explain (analyze %t) of a scan of 1200 partitions: isError %t, %d bytes:\n%.300s", analyze, isErr, len(got), got)
		}
	}
}
