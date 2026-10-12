package explain

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fixtures in testdata are the output of EXPLAIN (FORMAT JSON, VERBOSE),
// with ANALYZE and BUFFERS for the analyze_ ones, from PostgreSQL 17 on the
// app fixture database (internal/testdb), with these settings and queries:
//
//   - plan.json, enable_hashjoin and enable_mergejoin off:
//     SELECT e.id, c.email FROM app.events e JOIN app.customers c ON c.id = (e.payload->>'customer')::uuid
//   - analyze_sort.json, work_mem 64kB:
//     SELECT id, payload FROM app.events WHERE id % 4 = 0 AND kind = 'view' ORDER BY payload
//   - analyze_hash.json, work_mem 64kB, enable_sort and enable_mergejoin off:
//     SELECT a.payload, count(*) FROM app.events a JOIN app.events b ON a.payload = b.payload GROUP BY a.payload
//   - analyze_parallel.json, parallel_setup_cost, parallel_tuple_cost and
//     min_parallel_table_scan_size 0, enable_hashjoin and enable_mergejoin off:
//     SELECT e.kind, count(*) FROM app.events e JOIN app.customers c ON c.id = (e.payload->>'customer')::uuid GROUP BY e.kind

// fixtureSizes are the reltuples of the app fixture's relations.
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
		// The outer side's estimate is enough to flag the nested loop; its
		// own cost is mostly the inner index scan, run once per outer row.
		{"plan", `total cost 3842.28, ~5000 rows
top nodes by own cost:
  Nested Loop: cost 3314.11, ~5000 rows
  Seq Scan on app.events e: cost 528, ~20000 rows
  Index Scan using customers_pkey on app.customers c: cost 0.17, ~1 row
flags:
  Nested Loop: outer side of 20000 rows, each running the inner side
  Seq Scan on app.events e: full scan of ~20000 rows`},
		// The planner multiplies the two filters' selectivities, but kind is
		// 'view' exactly when id % 4 = 0: 200 times the estimate.
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
		// Times add up over loops and workers: 20000 inner loops of 0.001 ms,
		// as EXPLAIN rounds them, exceed the 14 ms the query took.
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
		"analyze_hash":     {{"app", "events"}}, // scanned twice
		"analyze_parallel": {{"app", "events"}}, // the parallel scan
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

// node parses one plan node from JSON.
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
	if got := rows(1) + ", " + rows(0) + ", " + rows(2); got != "1 row, 0 rows, 2 rows" {
		t.Errorf("rows = %q", got)
	}
}
