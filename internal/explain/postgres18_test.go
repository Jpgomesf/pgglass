package explain

import (
	"slices"
	"testing"
)

// TestSummaryOfAPostgreSQL18Plan reads testdata/analyze_window.json, the
// output of EXPLAIN (FORMAT JSON, VERBOSE, ANALYZE, BUFFERS) from PostgreSQL
// 18 on the app fixture, with work_mem 64kB and parallel_setup_cost,
// parallel_tuple_cost and min_parallel_table_scan_size 0, of
// SELECT e.kind, count(*) OVER () FROM app.events e. PostgreSQL 18 gives the
// rows of each loop to two decimals, and reports where a window aggregate
// kept its rows.
func TestSummaryOfAPostgreSQL18Plan(t *testing.T) {
	want := `total cost 661.33, 11.478 ms, 20000 rows, 328 buffers hit, 0 read
top nodes by own time:
  WindowAgg: 5.972 ms, 20000 rows
  Parallel Seq Scan on app.events e: 4.644 ms, 6666.67 rows × 3 loops
  Gather: 0 ms, 20000 rows
flags:
  WindowAgg: spilled to disk (328 kB)
  Parallel Seq Scan on app.events e: full scan of ~20000 rows`
	if got := fixture(t, "analyze_window").summary(fixtureSizes); got != want {
		t.Errorf("analyze_window:\n%s\nwant\n%s", got, want)
	}
}

// From PostgreSQL 18, a window aggregate, materialize, CTE scan and the like
// report the storage of their rows. Every scan of one CTE reports the CTE's,
// even one that never ran.
func TestFlagsOfStorage(t *testing.T) {
	for _, tc := range []struct {
		name, node string
		want       []string
	}{
		{"window aggregate on disk", `{"Node Type": "WindowAgg", "Storage": "Disk", "Maximum Storage": 328, "Actual Loops": 1}`,
			[]string{"spilled to disk (328 kB)"}},
		{"materialize on disk", `{"Node Type": "Materialize", "Storage": "Disk", "Maximum Storage": 6836, "Actual Loops": 3}`,
			[]string{"spilled to disk (6836 kB)"}},
		{"window aggregate in memory", `{"Node Type": "WindowAgg", "Storage": "Memory", "Maximum Storage": 17, "Actual Loops": 1}`, nil},
		{"CTE scan never executed", `{"Node Type": "CTE Scan", "Storage": "Disk", "Maximum Storage": 6832, "Actual Loops": 0}`, nil},
	} {
		if got := parseNode(t, tc.node).flags(true, nil); !slices.Equal(got, tc.want) {
			t.Errorf("%s: flags = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// From PostgreSQL 18, the rows of each loop have two decimals, which the
// summary keeps, as EXPLAIN does: 0.8 rows over 20000 loops are 16000 rows,
// not 20000.
func TestRowsPerLoopKeepTwoDecimals(t *testing.T) {
	limit := parseNode(t, `{"Node Type": "Limit", "Plan Rows": 1, "Actual Total Time": 0.008, "Actual Rows": 0.80, "Actual Loops": 20000}`)
	if got, want := limit.measure(true), "160 ms, 0.8 rows × 20000 loops"; got != want {
		t.Errorf("measure = %q, want %q", got, want)
	}
	result := parseNode(t, `{"Node Type": "Result", "Plan Rows": 25, "Actual Rows": 0.5, "Actual Loops": 4}`)
	if got, want := result.flags(true, nil), []string{"0.5 rows, planned 25"}; !slices.Equal(got, want) {
		t.Errorf("flags = %q, want %q", got, want)
	}
}
