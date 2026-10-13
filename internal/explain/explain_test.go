package explain_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/explain"
	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// recorder is a Querier that records the query it is given and fails it, so
// Run stops there.
type recorder struct{ got engine.Query }

var errRecorded = errors.New("recorded")

func (r *recorder) Query(_ context.Context, q engine.Query) (*result.Result, error) {
	r.got = q
	return nil, errRecorded
}

// Run sends the EXPLAIN that New puts around the SELECT, with the
// search_path it is given.
func TestRunSendsTheExplain(t *testing.T) {
	for _, tc := range []struct {
		sql     string
		analyze bool
		want    string
	}{
		{"SELECT 1", false, "EXPLAIN (FORMAT JSON, VERBOSE) SELECT 1"},
		{"SELECT 1", true, "EXPLAIN (FORMAT JSON, VERBOSE, ANALYZE, BUFFERS) SELECT 1"},
		{"(SELECT 1) UNION ALL VALUES (2)", false, "EXPLAIN (FORMAT JSON, VERBOSE) (SELECT 1) UNION ALL VALUES (2)"},
		{"SELECT 1 -- trailing comment", false, "EXPLAIN (FORMAT JSON, VERBOSE) SELECT 1 -- trailing comment"},
	} {
		stmt, err := explain.New(tc.sql, tc.analyze)
		if err != nil {
			t.Errorf("New(%q, %v): %v", tc.sql, tc.analyze, err)
			continue
		}
		var q recorder
		if _, err := stmt.Run(t.Context(), &q, "app"); !errors.Is(err, errRecorded) {
			t.Errorf("Run of %q = %v, want the querier's error", tc.sql, err)
		}
		if want := (engine.Query{SQL: tc.want, SearchPath: "app"}); !reflect.DeepEqual(q.got, want) {
			t.Errorf("New(%q, %v) then Run sent %+v, want %+v", tc.sql, tc.analyze, q.got, want)
		}
	}
}

// What pg_query rejects is rejected with the same reason; of what it
// allows, EXPLAIN and SHOW cannot be explained.
func TestNewRejects(t *testing.T) {
	for sql, want := range map[string]string{
		"DELETE FROM app.events":                                               "DeleteStmt is not allowed: only SELECT, EXPLAIN SELECT and SHOW are",
		"EXPLAIN DELETE FROM app.events":                                       "EXPLAIN of DeleteStmt is not allowed",
		"EXPLAIN ANALYZE DELETE FROM app.events":                               "EXPLAIN of DeleteStmt is not allowed",
		"SELECT 1; DELETE FROM app.events":                                     "SQL must be exactly one statement, got 2",
		"SELECT pg_read_file('/etc/passwd')":                                   "function pg_read_file is not allowed",
		"WITH gone AS (DELETE FROM app.events RETURNING *) SELECT * FROM gone": "WITH query \"gone\" is a DeleteStmt",
		"SELECT * FROM app.events FOR UPDATE":                                  "FOR UPDATE/FOR SHARE is not allowed",
		"EXPLAIN SELECT 1":                                                     "only a SELECT can be explained, not EXPLAIN or SHOW",
		"EXPLAIN (ANALYZE) SELECT 1":                                           "only a SELECT can be explained, not EXPLAIN or SHOW",
		"SHOW work_mem":                                                        "only a SELECT can be explained, not EXPLAIN or SHOW",
		"":                                                                     "SQL must be exactly one statement, got 0",
	} {
		for _, analyze := range []bool{false, true} {
			_, err := explain.New(sql, analyze)
			var v *guard.Violation
			if !errors.As(err, &v) || !strings.Contains(v.Reason, want) {
				t.Errorf("New(%q, %v) = %v, want a *guard.Violation containing %q", sql, analyze, err, want)
			}
		}
	}
}

// run explains sql through q.
func run(t *testing.T, q engine.Querier, sql string, analyze bool) (string, error) {
	t.Helper()
	stmt, err := explain.New(sql, analyze)
	if err != nil {
		t.Fatal(err)
	}
	return stmt.Run(t.Context(), q, "")
}

// summaryOf runs sql and checks its summary against the lines of want, each
// a regular expression.
func summaryOf(t *testing.T, q engine.Querier, sql string, analyze bool, want ...string) {
	t.Helper()
	got, err := run(t, q, sql, analyze)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if re := regexp.MustCompile(`^` + strings.Join(want, `\n`) + `$`); !re.MatchString(got) {
		t.Errorf("%s (analyze %v):\n%s\nwant it to match\n%s", sql, analyze, got, re)
	}
}

const (
	cost = `[0-9.]+`
	ms   = `[0-9.]+ ms`
)

// kind has no index: the filter reads the 20k rows of app.events.
func TestUnindexedFilterIsFlaggedAsASeqScan(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	const sql = "SELECT id FROM app.events WHERE kind = 'click'"
	summaryOf(t, q, sql, false,
		`total cost `+cost+`, ~5000 rows`,
		`top nodes by own cost:`,
		`  Seq Scan on app.events: cost `+cost+`, ~5000 rows`,
		`flags:`,
		`  Seq Scan on app.events: full scan of ~20000 rows`)
	summaryOf(t, q, sql, true,
		`total cost `+cost+`, `+ms+`, 5000 rows, \d+ buffers hit, \d+ read`,
		`top nodes by own time:`,
		`  Seq Scan on app.events: `+ms+`, 5000 rows`,
		`flags:`,
		`  Seq Scan on app.events: full scan of ~20000 rows`)
}

// flagsOf runs sql and returns the flag lines of its summary.
func flagsOf(t *testing.T, q engine.Querier, sql string, analyze bool) []string {
	t.Helper()
	got, err := run(t, q, sql, analyze)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	_, flags, ok := strings.Cut(got, "\nflags:\n")
	if !ok {
		t.Fatalf("%s: no flags in\n%s", sql, got)
	}
	return strings.Split(flags, "\n")
}

// wantFlags checks that each of want, a regular expression, matches one of
// flags in order.
func wantFlags(t *testing.T, flags []string, want ...string) {
	t.Helper()
	if len(flags) != len(want) {
		t.Fatalf("flags:\n%s\nwant %d", strings.Join(flags, "\n"), len(want))
	}
	for i, w := range want {
		if !regexp.MustCompile(`^  ` + w + `$`).MatchString(flags[i]) {
			t.Errorf("flag %d = %q, want it to match %q", i, flags[i], w)
		}
	}
}

// The planner multiplies the selectivities of the two filters, but kind is
// 'view' exactly when id % 4 = 0: 5000 rows where it plans 25.
func TestAnalyzeFlagsAMissedEstimate(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	wantFlags(t, flagsOf(t, q, "SELECT id FROM app.events WHERE id % 4 = 0 AND kind = 'view'", true),
		`Seq Scan on app.events: 5000 rows, planned 25`,
		`Seq Scan on app.events: full scan of ~20000 rows`)
}

// 500k rows do not fit in the default work_mem of 4MB.
func TestAnalyzeFlagsASortThatSpilled(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	wantFlags(t, flagsOf(t, q, "SELECT g FROM generate_series(1, 500000) g ORDER BY g DESC", true),
		`Sort: spilled to disk \(external merge, \d+ kB\)`)
}

// A LATERAL aggregate runs once per outer row, in a nested loop over the 20k
// events.
func TestNestedLoopOverManyRowsIsFlagged(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	const sql = `SELECT e.id, x.n FROM app.events e CROSS JOIN LATERAL
		(SELECT count(*) AS n FROM app.customers c WHERE c.id = (e.payload->>'customer')::uuid) x`
	for _, analyze := range []bool{false, true} {
		wantFlags(t, flagsOf(t, q, sql, analyze),
			`Nested Loop: outer side of 20000 rows, each running the inner side`,
			`Seq Scan on app.events e: full scan of ~20000 rows`)
	}
}

// A Limit stops the seq scan after 10 rows: the scan costs what the Limit
// reads of it, and nothing is flagged; nor is a join under the Limit.
func TestALimitStopsTheScanEarly(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	const sql = "SELECT id FROM app.events LIMIT 10"
	summaryOf(t, q, sql, false,
		`total cost `+cost+`, ~10 rows`,
		`top nodes by own cost:`,
		`  Seq Scan on app\.events: cost `+cost+`, ~10 rows`,
		`  Limit: cost 0, ~10 rows`,
		`flags: none`)
	summaryOf(t, q, sql, true,
		`total cost `+cost+`, `+ms+`, 10 rows, \d+ buffers hit, \d+ read`,
		`top nodes by own time:`,
		`  (Seq Scan on app\.events|Limit): `+ms+`, 10 rows`,
		`  (Seq Scan on app\.events|Limit): `+ms+`, 10 rows`,
		`flags: none`)

	const join = "SELECT e.id, c.email FROM app.events e JOIN app.customers c ON c.id = (e.payload->>'customer')::uuid LIMIT 10"
	for _, analyze := range []bool{false, true} {
		if got, err := run(t, q, join, analyze); err != nil || !strings.HasSuffix(got, "\nflags: none") {
			t.Errorf("%s (analyze %v): %v\n%s", join, analyze, err, got)
		}
	}
}

// A Limit does not hide a scan that reads the whole relation: one that finds
// no row to return, one under a sort, which reads every row before the Limit
// stops it, and one that the offset makes read 15010 rows.
func TestALimitStillFlagsAFullScan(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	for _, sql := range []string{
		"SELECT id FROM app.events WHERE kind = 'none' LIMIT 10",
		"SELECT id FROM app.events ORDER BY payload LIMIT 10",
		"SELECT id FROM app.events OFFSET 15000 LIMIT 10",
	} {
		for _, analyze := range []bool{false, true} {
			wantFlags(t, flagsOf(t, q, sql, analyze), `Seq Scan on app\.events: full scan of ~20000 rows`)
		}
	}
}

// Neither a lookup by primary key nor a seq scan of a small table is
// flagged.
func TestSmallAndIndexedScansAreNotFlagged(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	summaryOf(t, q, "SELECT * FROM app.events WHERE id = 42", true,
		`total cost `+cost+`, `+ms+`, 1 row, \d+ buffers hit, \d+ read`,
		`top nodes by own time:`,
		`  Index Scan using events_pkey on app.events: `+ms+`, 1 row`,
		`flags: none`)
	summaryOf(t, q, "SELECT * FROM app.customers WHERE name = 'Customer 7'", false,
		`total cost `+cost+`, ~1 row`,
		`top nodes by own cost:`,
		`  Seq Scan on app.customers: cost `+cost+`, ~1 row`,
		`flags: none`)
}

// The search_path resolves unqualified names, and the seq scan is still
// flagged by its schema.
func TestRunUsesTheSearchPath(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	stmt, err := explain.New("SELECT count(*) FROM events WHERE kind = 'view'", false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := stmt.Run(t.Context(), q, "app")
	if err != nil || !strings.HasSuffix(got, "\n  Seq Scan on app.events: full scan of ~20000 rows") {
		t.Errorf("search_path app: %v\n%s", err, got)
	}
	_, err = stmt.Run(t.Context(), q, "public")
	var qe *engine.QueryError
	if !errors.As(err, &qe) || qe.SQLState != "42P01" {
		t.Errorf("search_path public: %v, want undefined_table", err)
	}
}

// A Postgres error points into the SELECT as the caller wrote it, not into
// the EXPLAIN around it.
func TestErrorPositionsPointIntoTheSelect(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	for _, analyze := range []bool{false, true} {
		_, err := run(t, q, "SELECT nam FROM app.customers", analyze)
		var qe *engine.QueryError
		if !errors.As(err, &qe) || qe.SQLState != "42703" || qe.Position != 8 {
			t.Errorf("analyze %v: %#v, want undefined_column at character 8", analyze, err)
		}
	}
}

// limited returns a Querier on the app fixture, as ro_clean, with defaults
// changed by set.
func limited(t *testing.T, set func(*config.Defaults)) engine.Querier {
	t.Helper()
	defaults := config.Defaults{}.WithDefaults()
	set(&defaults)
	m := engine.New(&config.Config{
		Defaults:    defaults,
		Connections: map[string]config.Connection{"db": {Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, testdb.DBApp)}},
	})
	t.Cleanup(m.Close)
	return m.Querier("db")
}

// ANALYZE runs the query, so the statement timeout ends it; without
// ANALYZE, nothing runs.
func TestAnalyzeIsBoundByTheStatementTimeout(t *testing.T) {
	q := limited(t, func(d *config.Defaults) { d.StatementTimeoutSeconds = 1 })
	const sql = "SELECT pg_sleep(30)"

	start := time.Now()
	if _, err := run(t, q, sql, false); err != nil {
		t.Errorf("plan only: %v", err)
	}
	_, err := run(t, q, sql, true)
	var qe *engine.QueryError
	if !errors.As(err, &qe) || qe.SQLState != "57014" || qe.Message != "canceling statement due to statement timeout" {
		t.Errorf("ANALYZE of %s = %v, want a statement timeout (57014)", sql, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v: the timeout did not end the query", elapsed)
	}
}

// ANALYZE runs the query inside the engine's READ ONLY transaction: a
// function that writes fails, and the table stays empty.
func TestAnalyzeRunsReadOnly(t *testing.T) {
	const name = "explain_writes"
	testdb.CreateDatabase(t, name, `CREATE TABLE t (v int);
		CREATE FUNCTION bump() RETURNS int LANGUAGE sql VOLATILE AS $$ INSERT INTO t VALUES (1) RETURNING v $$;`)
	q := testdb.Querier(t, testdb.Superuser, name)

	_, err := run(t, q, "SELECT bump()", true)
	var qe *engine.QueryError
	if !errors.As(err, &qe) || qe.SQLState != "25006" {
		t.Errorf("ANALYZE of a writing function = %v, want read_only_sql_transaction (25006)", err)
	}
	if n := testdb.Value[int](t, testdb.Conn(t, testdb.Superuser, name), "SELECT count(*) FROM t"); n != 0 {
		t.Errorf("t has %d rows after EXPLAIN ANALYZE", n)
	}
}

func TestPlanLargerThanMaxBytesIsAnError(t *testing.T) {
	q := limited(t, func(d *config.Defaults) { d.MaxBytes = 100 })
	_, err := run(t, q, "SELECT id FROM app.events", false)
	if err == nil || err.Error() != "read the plan: it exceeds the configured limits (max_bytes=100)" {
		t.Errorf("err = %v", err)
	}
}
