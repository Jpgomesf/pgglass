package explain

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var fixtureSizes = map[relation]float64{{"app", "events"}: 20000, {"app", "customers"}: 50}

func fixture(t *testing.T, name string) *plan {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := parse(string(doc))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

func TestSummaryOfFixtures(t *testing.T) {
	for _, tc := range []struct{ fixture, want string }{
		{"plan", `total cost 3842.28, ~5000 rows
top nodes by own cost:
  Nested Loop: cost 3314.11, ~5000 rows
  Seq Scan on app.events e: cost 528, ~20000 rows
  Index Scan using customers_pkey on app.customers c: cost 0.17, ~1 row
flags:
  Nested Loop: outer side of 20000 rows, each running the inner side
  Seq Scan on app.events e: full scan of ~20000 rows`},
		{"analyze_sort", `total cost 678.64, 5.496 ms, 5000 rows, 333 buffers hit, 0 read
top nodes by own time:
  Sort: 3.195 ms, 5000 rows
  Seq Scan on app.events: 2.087 ms, 5000 rows
flags:
  Sort: 5000 rows, planned 25
  Sort: spilled to disk (external merge, 504 kB)
  Seq Scan on app.events: 5000 rows, planned 25
  Seq Scan on app.events: full scan of ~20000 rows`},
		{"analyze_hash", `total cost 11714.19, 30.57 ms, 20000 rows, 656 buffers hit, 0 read
top nodes by own time:
  Hash Join: 11.948 ms, 20000 rows
  Aggregate: 11.236 ms, 20000 rows
  Hash: 4.139 ms, 20000 rows
  Seq Scan on app.events b: 1.25 ms, 20000 rows
  Seq Scan on app.events a: 1.235 ms, 20000 rows
flags:
  Aggregate: spilled to disk (3352 kB)
  Seq Scan on app.events a: full scan of ~20000 rows
  Hash: spilled to disk (32 batches)
  Seq Scan on app.events b: full scan of ~20000 rows`},
		{"analyze_parallel", `total cost 1802.91, 14.452 ms, 4 rows, 40346 buffers hit, 0 read
top nodes by own time:
  Index Only Scan using customers_pkey on app.customers c: 20 ms, 1 row × 20000 loops
  Nested Loop: 11.149 ms, 6667 rows × 3 loops
  Aggregate: 2.424 ms, 4 rows × 3 loops
  Parallel Seq Scan on app.events e: 1.323 ms, 6667 rows × 3 loops
  Sort: 0.051 ms, 4 rows × 3 loops
flags:
  Nested Loop: outer side of 20001 rows, each running the inner side
  Parallel Seq Scan on app.events e: full scan of ~20000 rows`},
		{"limit", `total cost 0.26, ~10 rows
top nodes by own cost:
  Seq Scan on app.events: cost 0.26, ~10 rows
  Limit: cost 0, ~10 rows
flags: none`},
		{"analyze_limit", `total cost 0.26, 0.022 ms, 10 rows, 2 buffers hit, 0 read
top nodes by own time:
  Seq Scan on app.events: 0.013 ms, 10 rows
  Limit: 0.002 ms, 10 rows
flags: none`},
		{"analyze_limit_sort", `total cost 960.22, 4.806 ms, 10 rows, 333 buffers hit, 0 read
top nodes by own time:
  Sort: 3.255 ms, 10 rows
  Seq Scan on app.events: 1.539 ms, 20000 rows
  Limit: 0.002 ms, 10 rows
flags:
  Seq Scan on app.events: full scan of ~20000 rows`},
		{"limit_nested", `total cost 7.83, ~10 rows
top nodes by own cost:
  Nested Loop: cost 6.61, ~10 rows
  Seq Scan on app.events e: cost 1.06, ~40 rows
  Index Scan using customers_pkey on app.customers c: cost 0.17, ~1 row
  Limit: cost 0, ~10 rows
flags: none`},
	} {
		if got := fixture(t, tc.fixture).summary(fixtureSizes); got != tc.want {
			t.Errorf("%s:\n%s\nwant\n%s", tc.fixture, got, tc.want)
		}
	}
}

// Without sizes, as for relations never analyzed (reltuples -1), no seq
// scan is flagged.
func TestSummaryWithoutSizes(t *testing.T) {
	got := fixture(t, "plan").summary(map[relation]float64{{"app", "events"}: -1})
	if strings.Contains(got, "full scan") {
		t.Errorf("a relation of unknown size is flagged:\n%s", got)
	}
}

func TestSeqScansNamesEachRelationOnce(t *testing.T) {
	for name, want := range map[string][]relation{
		"plan":             {{"app", "events"}},
		"analyze_hash":     {{"app", "events"}},
		"analyze_parallel": {{"app", "events"}},
	} {
		if got := fixture(t, name).seqScans(); !slices.Equal(got, want) {
			t.Errorf("%s: seqScans = %v, want %v", name, got, want)
		}
	}
}

// Under Gather, the workers' time adds up past the leader's: own time stops
// at zero.
func TestOwnTimeIsNeverNegative(t *testing.T) {
	gather := &fixture(t, "analyze_parallel").Root.Plans[0]
	if gather.Type != "Gather Merge" || gather.total(true) >= gather.Plans[0].total(true) {
		t.Fatalf("fixture changed: %s takes %v ms, its child %v ms", gather.Type, gather.total(true), gather.Plans[0].total(true))
	}
	if own := gather.own(true); own != 0 {
		t.Errorf("own time of %s = %v, want 0", gather.Type, own)
	}
}

func parseNode(t *testing.T, doc string) *node {
	t.Helper()
	p, err := parse(`[{"Plan": ` + doc + `}]`)
	if err != nil {
		t.Fatal(err)
	}
	return &p.Root
}

func TestFlags(t *testing.T) {
	sizes := map[relation]float64{{"s", "big"}: 10_001, {"s", "edge"}: 10_000, {"s", "fresh"}: -1}
	for _, tc := range []struct {
		name     string
		node     string
		analyzed bool
		want     []string
	}{
		{"seq scan over 10k rows", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big"}`, false,
			[]string{"full scan of ~10001 rows"}},
		{"seq scan of 10k rows", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "edge"}`, false, nil},
		{"seq scan never analyzed", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "fresh"}`, false, nil},
		{"seq scan that read 10 rows", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big",
			"Plan Rows": 10, "Actual Rows": 10, "Actual Loops": 1}`, true, nil},
		{"seq scan that filtered out 10k rows", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big",
			"Plan Rows": 1, "Actual Rows": 1, "Rows Removed by Filter": 10000, "Actual Loops": 1}`, true,
			[]string{"full scan of ~10001 rows"}},
		{"seq scan that read 10k rows in 2 loops", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big",
			"Plan Rows": 1, "Actual Rows": 1, "Rows Removed by Filter": 4999, "Actual Loops": 2}`, true, nil},
		{"parallel seq scan that read over 10k rows in 3 loops", `{"Node Type": "Seq Scan", "Parallel Aware": true, "Schema": "s", "Relation Name": "big",
			"Plan Rows": 1, "Actual Rows": 0, "Rows Removed by Filter": 3334, "Actual Loops": 3}`, true,
			[]string{"full scan of ~10001 rows"}},
		{"seq scan never executed", `{"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big",
			"Plan Rows": 1, "Actual Rows": 0, "Actual Loops": 0}`, true, nil},
		{"index scan of a big relation", `{"Node Type": "Index Scan", "Schema": "s", "Relation Name": "big"}`, false, nil},
		{"11 times the estimate", `{"Node Type": "Result", "Plan Rows": 1, "Actual Rows": 11, "Actual Loops": 1}`, true,
			[]string{"11 rows, planned 1"}},
		{"10 times the estimate", `{"Node Type": "Result", "Plan Rows": 1, "Actual Rows": 10, "Actual Loops": 1}`, true, nil},
		{"a tenth of the estimate", `{"Node Type": "Result", "Plan Rows": 1000, "Actual Rows": 100, "Actual Loops": 2}`, true, nil},
		{"under a tenth", `{"Node Type": "Result", "Plan Rows": 1000, "Actual Rows": 99, "Actual Loops": 2}`, true,
			[]string{"99 rows, planned 1000"}},
		{"no rows where one was planned", `{"Node Type": "Result", "Plan Rows": 1, "Actual Rows": 0, "Actual Loops": 1}`, true, nil},
		{"never executed", `{"Node Type": "Result", "Plan Rows": 1000, "Actual Rows": 0, "Actual Loops": 0}`, true, nil},
		{"estimate without ANALYZE", `{"Node Type": "Result", "Plan Rows": 1000}`, false, nil},
		{"sort in memory", `{"Node Type": "Sort", "Sort Method": "quicksort", "Sort Space Used": 25, "Sort Space Type": "Memory"}`, true, nil},
		{"sort spilled by a worker", `{"Node Type": "Sort", "Sort Method": "quicksort", "Sort Space Type": "Memory",
			"Workers": [{"Worker Number": 0, "Sort Method": "quicksort", "Sort Space Type": "Memory"},
				{"Worker Number": 1, "Sort Method": "external merge", "Sort Space Used": 900, "Sort Space Type": "Disk"}]}`, true,
			[]string{"spilled to disk (external merge, 900 kB)"}},
		{"hash in one batch", `{"Node Type": "Hash", "Hash Batches": 1}`, true, nil},
		{"hash aggregate in memory", `{"Node Type": "Aggregate", "HashAgg Batches": 1, "Disk Usage": 0}`, true, nil},
		{"nested loop over 10k outer rows", `{"Node Type": "Nested Loop", "Plans": [
			{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Plan Rows": 10000, "Actual Rows": 2500, "Actual Loops": 5},
			{"Node Type": "Index Scan", "Parent Relationship": "Inner", "Plan Rows": 10001}]}`, true,
			[]string{"outer side of 12500 rows, each running the inner side"}},
		{"nested loop over 10k planned outer rows", `{"Node Type": "Nested Loop", "Plans": [
			{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Plan Rows": 10000},
			{"Node Type": "Index Scan", "Parent Relationship": "Inner", "Plan Rows": 10001}]}`, false, nil},
	} {
		if got := parseNode(t, tc.node).flags(tc.analyzed, sizes); !slices.Equal(got, tc.want) {
			t.Errorf("%s: flags = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func planNode(fields string, startup, total, rows float64, children ...string) string {
	return fmt.Sprintf(`{%s, "Startup Cost": %v, "Total Cost": %v, "Plan Rows": %v, "Plans": [%s]}`,
		fields, startup, total, rows, strings.Join(children, ", "))
}

// A Limit reads its input only as far as the rows it skips and returns, and an
// EXISTS up to its first row; the plans are PostgreSQL 17's, trimmed.
func TestShare(t *testing.T) {
	limit := func(startup, total, rows float64, children ...string) string {
		return planNode(`"Node Type": "Limit"`, startup, total, rows, children...)
	}
	outer := func(fields string, startup, total, rows float64, children ...string) string {
		return planNode(fields+`, "Parent Relationship": "Outer"`, startup, total, rows, children...)
	}
	exists := func(parent string, total, rows float64) string {
		return planNode(`"Node Type": "Seq Scan", "Parent Relationship": "`+parent+`", "Plan Width": 0`, 0, total, rows)
	}
	scan := outer(`"Node Type": "Seq Scan"`, 0, 528, 20000)
	skipped := (396.12-578.0/5000)/528 + 10.0/20000
	for _, tc := range []struct {
		name, plan string
		want       []float64
	}{
		{"LIMIT 10", limit(0, 0.26, 10, scan), []float64{1, 0.0005}},
		{"OFFSET 15000 LIMIT 10", limit(396, 396.26, 10, scan), []float64{1, 0.7505}},
		{"a LIMIT over fewer rows", limit(0, 578, 1, outer(`"Node Type": "Seq Scan"`, 0, 578, 1)), []float64{1, 1}},
		{"nested limits", limit(0, 0.26, 10, outer(`"Node Type": "Limit"`, 0, 2.64, 100, scan)), []float64{1, 0.1, 0.0005}},
		{"nested limits with an offset", limit(396, 396.13, 5, outer(`"Node Type": "Limit"`, 396, 398.64, 100, scan)), []float64{1, 0.05, 0.75025}},
		{"an initplan", limit(578.01, 578.95, 10,
			planNode(`"Node Type": "Aggregate", "Strategy": "Plain", "Parent Relationship": "InitPlan"`, 578, 578.01, 1, scan),
			outer(`"Node Type": "Seq Scan"`, 0, 628, 6667)), []float64{1, 1, 1, 10.0 / 6667}},
		{"a CTE", limit(528, 528.2, 10,
			planNode(`"Node Type": "Seq Scan", "Parent Relationship": "InitPlan", "Subplan Name": "CTE x"`, 0, 528, 20000),
			outer(`"Node Type": "CTE Scan"`, 0, 400, 20000)), []float64{1, 1, 0.0005}},
		{"a sort", limit(960.19, 960.22, 10, outer(`"Node Type": "Sort"`, 960.19, 1010.19, 20000, scan)), []float64{1, 0.0005, 1}},
		{"a sorted aggregate", limit(0.29, 0.87, 10,
			outer(`"Node Type": "Aggregate", "Strategy": "Sorted"`, 0.29, 1159.29, 20000,
				outer(`"Node Type": "Index Only Scan"`, 0.29, 859.29, 20000))), []float64{1, 0.0005, 0.0005}},
		{"a hashed set operation", limit(0, 0.77, 10,
			outer(`"Node Type": "SetOp", "Strategy": "Hashed"`, 0, 1543.5, 20000,
				outer(`"Node Type": "Append"`, 0, 1481, 25000))), []float64{1, 0.0005, 1}},
		{"a bitmap heap scan", limit(99.03, 99.81, 10,
			outer(`"Node Type": "Bitmap Heap Scan"`, 99.03, 489.52, 4999,
				outer(`"Node Type": "Bitmap Index Scan"`, 0, 97.78, 4999))), []float64{1, 10.0 / 4999, 1}},
		{"a hash join", limit(3.12, 4.3, 10,
			outer(`"Node Type": "Hash Join"`, 3.12, 592.58, 5000, scan,
				planNode(`"Node Type": "Hash", "Parent Relationship": "Inner"`, 2.5, 2.5, 50,
					outer(`"Node Type": "Seq Scan"`, 0, 2.5, 50)))), []float64{1, 0.002, 0.002, 1, 1}},
		{"a nested loop", limit(0.15, 7.83, 10,
			outer(`"Node Type": "Nested Loop"`, 0.15, 3842.28, 5000, scan,
				planNode(`"Node Type": "Index Scan", "Parent Relationship": "Inner"`, 0.15, 0.17, 1))), []float64{1, 0.002, 0.002, 1}},
		{"a merge join", limit(0.57, 1.58, 10,
			outer(`"Node Type": "Merge Join"`, 0.57, 2018.58, 20000,
				outer(`"Node Type": "Index Only Scan"`, 0.29, 859.29, 20000),
				planNode(`"Node Type": "Index Only Scan", "Parent Relationship": "Inner"`, 0.29, 859.29, 20000))),
			[]float64{1, 0.0005, 0.0005, 0.0005}},
		{"a subplan", limit(0, 30.26, 10,
			outer(`"Node Type": "Seq Scan"`, 0, 60528, 20000,
				planNode(`"Node Type": "Seq Scan", "Parent Relationship": "SubPlan"`, 0, 3, 1))), []float64{1, 0.0005, 1}},
		{"a window over the whole partition", limit(777.96, 777.96, 10,
			outer(`"Node Type": "WindowAgg"`, 777.96, 778, 20000, scan)), []float64{1, 0.0005, 1}},
		{"a window that streams", limit(0, 0.39, 10,
			outer(`"Node Type": "WindowAgg"`, 0, 778, 20000, scan)), []float64{1, 0.0005, 0.0005}},
		{"a window with an initplan", limit(628.01, 628.4, 10,
			outer(`"Node Type": "WindowAgg"`, 628.01, 1406.01, 20000,
				planNode(`"Node Type": "Aggregate", "Strategy": "Plain", "Parent Relationship": "InitPlan"`, 628, 628.01, 1, scan),
				scan)), []float64{1, 0.0005, 1, 1, 0.0005}},
		{"an EXISTS initplan", planNode(`"Node Type": "Result"`, 0.12, 0.13, 1, exists("InitPlan", 578, 5000)), []float64{1, 0.0002}},
		{"an EXISTS subplan", planNode(`"Node Type": "Seq Scan"`, 0, 366.5, 50, exists("SubPlan", 728, 100)), []float64{1, 0.01}},
		{"an EXISTS planned to find one row", planNode(`"Node Type": "Result"`, 578, 578.01, 1, exists("InitPlan", 578, 1)), []float64{1, 1}},
		{"an ARRAY subplan", planNode(`"Node Type": "Seq Scan"`, 0, 36402.5, 50,
			planNode(`"Node Type": "Seq Scan", "Parent Relationship": "SubPlan", "Plan Width": 8`, 0, 728, 100)), []float64{1, 1}},
		{"a CTE that selects no column", planNode(`"Node Type": "Aggregate", "Strategy": "Plain"`, 978, 978.01, 1,
			planNode(`"Node Type": "Seq Scan", "Parent Relationship": "InitPlan", "Subplan Name": "CTE x", "Plan Width": 0`, 0, 528, 20000),
			outer(`"Node Type": "CTE Scan", "Plan Width": 0`, 0, 400, 20000)), []float64{1, 1, 1}},
		{"an EXISTS initplan of a Limit that skips rows", limit(396.12, 396.38, 10, exists("InitPlan", 578, 5000),
			outer(`"Node Type": "Result"`, 0, 528, 20000, scan)), []float64{1, 0.0002, skipped, skipped}},
	} {
		var got []float64
		for _, n := range parseNode(t, tc.plan).all() {
			got = append(got, n.share)
		}
		if !slices.EqualFunc(got, tc.want, func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }) {
			t.Errorf("%s: shares = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// What a Limit stops early returns fewer rows than planned, which is no
// misestimate, and a seq scan it stops reads only part of its relation.
func TestFlagsUnderALimit(t *testing.T) {
	sizes := map[relation]float64{{"s", "big"}: 20000}
	const scan = `"Node Type": "Seq Scan", "Schema": "s", "Relation Name": "big", "Plan Rows": 20000`
	for _, tc := range []struct {
		name     string
		node     string
		share    float64
		analyzed bool
		want     []string
	}{
		{"seq scan of 10 planned rows", `{` + scan + `}`, 0.0005, false, nil},
		{"seq scan of 15010 planned rows", `{` + scan + `}`, 0.7505, false, []string{"full scan of ~20000 rows"}},
		{"seq scan that returned 10 rows", `{` + scan + `, "Actual Rows": 10, "Actual Loops": 1}`, 0.0005, true, nil},
		{"seq scan that read every row and returned none", `{` + scan + `, "Actual Rows": 0, "Rows Removed by Filter": 20000, "Actual Loops": 1}`,
			0.0005, true, []string{"full scan of ~20000 rows"}},
		{"more rows than planned", `{"Node Type": "Index Scan", "Plan Rows": 1, "Actual Rows": 11, "Actual Loops": 1}`, 0.5, true,
			[]string{"11 rows, planned 1"}},
		{"nested loop of 40 planned outer rows", `{"Node Type": "Nested Loop", "Plans": [
			{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Plan Rows": 20000},
			{"Node Type": "Index Scan", "Parent Relationship": "Inner", "Plan Rows": 1}]}`, 0.002, false, nil},
	} {
		n := parseNode(t, tc.node)
		n.setShare(tc.share)
		if got := n.flags(tc.analyzed, sizes); !slices.Equal(got, tc.want) {
			t.Errorf("%s: flags = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// With ANALYZE, a scan a Limit is planned to stop early is not flagged for too
// few rows: EXPLAIN does not say why the Limit returned fewer.
func TestFlagsUnderALimitWithAnalyze(t *testing.T) {
	limit := func(actual, loops, startup, total, rows float64, children ...string) string {
		return planNode(fmt.Sprintf(`"Node Type": "Limit", "Parent Relationship": "Outer", "Actual Rows": %v, "Actual Loops": %v`, actual, loops),
			startup, total, rows, children...)
	}
	scan := func(actual, loops float64) string {
		return planNode(fmt.Sprintf(`"Node Type": "Index Scan", "Parent Relationship": "Outer", "Actual Rows": %v, "Actual Loops": %v`, actual, loops),
			0.29, 959.29, 1000)
	}
	for name, plan := range map[string]string{
		"a Limit that stopped the scan":        limit(10, 1, 0.29, 9.88, 10, scan(10, 1)),
		"a Limit that the Limit above stopped": limit(5, 1, 0.29, 5.09, 5, limit(5, 1, 0.29, 96.19, 100, scan(5, 1))),
		"a Limit with loops of fewer rows":     limit(9, 53, 0.29, 9.88, 10, scan(9, 53)),
		"a Limit that ran out of rows":         limit(3, 1, 0.29, 9.88, 10, scan(3, 1)),
	} {
		nodes := parseNode(t, plan).all()
		if got := nodes[len(nodes)-1].flags(true, nil); len(got) != 0 {
			t.Errorf("%s: the index scan is flagged %q", name, got)
		}
	}
}

func TestLabel(t *testing.T) {
	for doc, want := range map[string]string{
		`{"Node Type": "Seq Scan", "Schema": "app", "Relation Name": "events", "Alias": "events"}`:                    "Seq Scan on app.events",
		`{"Node Type": "Seq Scan", "Parallel Aware": true, "Schema": "app", "Relation Name": "events", "Alias": "e"}`: "Parallel Seq Scan on app.events e",
		`{"Node Type": "Index Only Scan", "Index Name": "orders_pkey", "Schema": "app", "Relation Name": "orders"}`:   "Index Only Scan using orders_pkey on app.orders",
		`{"Node Type": "Bitmap Index Scan", "Index Name": "orders_customer_id_idx"}`:                                  "Bitmap Index Scan using orders_customer_id_idx",
		`{"Node Type": "Seq Scan", "Relation Name": "events"}`:                                                        "Seq Scan on events",
		`{"Node Type": "Hash Join"}`: "Hash Join",
	} {
		if got := parseNode(t, doc).label(); got != want {
			t.Errorf("label of %s = %q, want %q", doc, got, want)
		}
	}
}

func TestMeasureOfANodeNeverExecuted(t *testing.T) {
	n := parseNode(t, `{"Node Type": "Index Scan", "Plan Rows": 1, "Actual Rows": 0, "Actual Loops": 0}`)
	if got := n.measure(true); got != "never executed" {
		t.Errorf("measure = %q", got)
	}
}

func TestParseRejectsWhatIsNotOnePlan(t *testing.T) {
	for doc, want := range map[string]string{
		`{"Plan": {}}`: "read the plan: json: cannot unmarshal object",
		`[]`:           "read the plan: got 0 plans, want 1",
		`[{}, {}]`:     "read the plan: got 2 plans, want 1",
		`QUERY PLAN`:   "read the plan: invalid character",
	} {
		if _, err := parse(doc); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("parse(%s) = %v, want an error starting %q", doc, err, want)
		}
	}
}

func TestNum(t *testing.T) {
	for _, tc := range []struct {
		v        float64
		decimals int
		want     string
	}{
		{528, 2, "528"},
		{3842.28, 2, "3842.28"},
		{30.570, 3, "30.57"},
		{0.0004, 3, "0"},
		{2.4239999999999995, 3, "2.424"},
		{20000, 0, "20000"},
		{6666.67, 0, "6667"},
	} {
		if got := num(tc.v, tc.decimals); got != tc.want {
			t.Errorf("num(%v, %d) = %q, want %q", tc.v, tc.decimals, got, tc.want)
		}
	}
	if got := rows(1, 0) + ", " + rows(0, 0) + ", " + rows(2, 0) + ", " + rows(0.8, 2) + ", " + rows(1, 2); got != "1 row, 0 rows, 2 rows, 0.8 rows, 1 row" {
		t.Errorf("rows = %q", got)
	}
}
