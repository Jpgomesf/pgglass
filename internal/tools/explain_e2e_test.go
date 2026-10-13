package tools_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func TestExplainToolDefinition(t *testing.T) {
	tool, size := listedTool(t, "pg_explain")
	if a := tool.Annotations; !a.ReadOnlyHint || *a.DestructiveHint || *a.OpenWorldHint {
		t.Errorf("annotations = %+v, want read-only, not destructive, closed world", a)
	}
	if size > 700 {
		t.Errorf("definition takes %d bytes of JSON, want at most 700", size)
	}
}

// The guard, the search_path check and argument errors come before the
// database: the DSN is never dialed.
func TestExplainRejectsBeforeTheDatabase(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckStrict, map[string]config.Connection{"db": direct(unusedDSN)}))
	const deleteSQL = "DELETE FROM app.events"
	cases := []struct {
		args          map[string]any
		want, outcome string
	}{
		{args("db", deleteSQL), "query rejected: DeleteStmt is not allowed: only SELECT, EXPLAIN SELECT and SHOW are", "guard_reject"},
		{args("db", "EXPLAIN DELETE FROM app.events", "analyze", true), "query rejected: EXPLAIN of DeleteStmt is not allowed", "guard_reject"},
		{args("db", "SELECT 1; DELETE FROM app.events"), "query rejected: SQL must be exactly one statement, got 2", "guard_reject"},
		{args("db", "SELECT lo_import('/etc/passwd')", "analyze", true), "query rejected: function lo_import is not allowed", "guard_reject"},
		{args("db", "EXPLAIN ANALYZE SELECT 1"), "query rejected: only a SELECT can be explained, not EXPLAIN or SHOW", "guard_reject"},
		{args("db", "SHOW work_mem"), "query rejected: only a SELECT can be explained, not EXPLAIN or SHOW", "guard_reject"},
		{args("db", "SELECT 1", "schema", "net"), "query rejected: schema net is not allowed as the search_path", "guard_reject"},
		{args("nope", "SELECT 1"), `unknown connection "nope" (available: db)`, "error"},
		{args("db", "SELECT 1", "analyze", "yes"), "invalid arguments: json: cannot unmarshal string", "error"},
		{args("db", "SELECT 1", "params", []any{1}), `invalid arguments: json: unknown field "params"`, "error"},
	}
	for _, tc := range cases {
		if got := s.fails(t, "pg_explain", tc.args); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%v:\n%s\nwant it to start with\n%s", tc.args, got, tc.want)
		}
	}

	lines := s.auditLines(t)
	if len(lines) != len(cases) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(cases), s.audit)
	}
	for i, line := range lines {
		if line.Tool != "pg_explain" || line.Outcome != cases[i].outcome {
			t.Errorf("audit line %d = %+v, want outcome %s", i, line, cases[i].outcome)
		}
	}
	if first := lines[0]; first.Connection != "db" || first.SQLSHA256 != sha12(deleteSQL) || first.SQLLen != len(deleteSQL) {
		t.Errorf("first audit line = %+v", first)
	}
	if strings.Contains(s.audit.String(), "events") {
		t.Errorf("audit log holds SQL:\n%s", s.audit)
	}
}

func TestExplainSummarizesThePlan(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	// kind has no index; default_schema app resolves events.
	const sql = "SELECT id FROM events WHERE kind = 'click'"
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{args("clean", sql), `total cost [0-9.]+, ~5000 rows
top nodes by own cost:
  Seq Scan on app\.events: cost [0-9.]+, ~5000 rows
flags:
  Seq Scan on app\.events: full scan of ~20000 rows`},
		{args("clean", sql, "analyze", true), `total cost [0-9.]+, [0-9.]+ ms, 5000 rows, \d+ buffers hit, \d+ read
top nodes by own time:
  Seq Scan on app\.events: [0-9.]+ ms, 5000 rows
flags:
  Seq Scan on app\.events: full scan of ~20000 rows`},
		{args("leaky", "SELECT * FROM app.events WHERE id = 7", "analyze", false), `role check: role ro_leaky: 3 finding\(s\)\n(?s:.*)\n\n` +
			`total cost [0-9.]+, ~1 row
top nodes by own cost:
  Index Scan using events_pkey on app\.events: cost [0-9.]+, ~1 row
flags: none`},
	} {
		got := s.ok(t, "pg_explain", tc.args)
		if !regexp.MustCompile(`^` + tc.want + `$`).MatchString(got) {
			t.Errorf("%v:\n%s\nwant it to match\n%s", tc.args, got, tc.want)
		}
	}

	// The schema argument replaces default_schema.
	if got := s.fails(t, "pg_explain", args("clean", sql, "schema", "public")); !strings.HasPrefix(got,
		`ERROR: relation "events" does not exist (SQLSTATE 42P01) at character 16`) {
		t.Errorf("events in public: %s", got)
	}
	// Positions point into the SELECT, as with pg_query.
	if got := s.fails(t, "pg_explain", args("clean", "SELECT nam FROM customers", "analyze", true)); !strings.HasPrefix(got,
		`ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8`) {
		t.Errorf("a misspelled column: %s", got)
	}

	want := []auditLine{
		{Msg: "tool call", Tool: "pg_explain", Connection: "clean", SQLSHA256: sha12(sql), SQLLen: len(sql), Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_explain", Connection: "clean", SQLSHA256: sha12(sql), SQLLen: len(sql), Outcome: "ok"},
	}
	lines := s.auditLines(t)
	if len(lines) != 5 {
		t.Fatalf("%d audit lines, want 5:\n%s", len(lines), s.audit)
	}
	for i, line := range lines[:2] {
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("line %d = %+v\nwant     %+v", i, line, want[i])
		}
	}
	if last := lines[4]; last.Outcome != "db_error" || last.SQLState != "42703" {
		t.Errorf("last audit line = %+v, want db_error 42703", last)
	}
}

// Under a LIMIT, the seq scan stops after 10 rows: it costs what the Limit
// reads of it, and nothing is flagged, with or without analyze.
func TestExplainUnderALimit(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	const sql = "SELECT id FROM events LIMIT 10"
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{args("clean", sql), `total cost [0-9.]+, ~10 rows
top nodes by own cost:
  Seq Scan on app\.events: cost [0-9.]+, ~10 rows
  Limit: cost 0, ~10 rows
flags: none`},
		{args("clean", sql, "analyze", true), `total cost [0-9.]+, [0-9.]+ ms, 10 rows, \d+ buffers hit, \d+ read
top nodes by own time:
  (Seq Scan on app\.events|Limit): [0-9.]+ ms, 10 rows
  (Seq Scan on app\.events|Limit): [0-9.]+ ms, 10 rows
flags: none`},
	} {
		got := s.ok(t, "pg_explain", tc.args)
		if !regexp.MustCompile(`^` + tc.want + `$`).MatchString(got) {
			t.Errorf("%v:\n%s\nwant it to match\n%s", tc.args, got, tc.want)
		}
	}
}

// The rows an offset skips are read in full, even under an outer Limit: the
// scan under it is flagged.
func TestExplainFlagsWhatALimitDoesNotStop(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	got := s.ok(t, "pg_explain", args("clean", "SELECT * FROM (SELECT id FROM events OFFSET 15000 LIMIT 100) s LIMIT 5"))
	want := `(?s:.*)\n  Seq Scan on app\.events: cost [0-9.]+, ~15005 rows\n(?s:.*)\nflags:\n  Seq Scan on app\.events: full scan of ~20000 rows`
	if !regexp.MustCompile(`^` + want + `$`).MatchString(got) {
		t.Errorf("%s\nwant it to match\n%s", got, want)
	}
}

// With analyze, the scan under a Limit that runs once per customer returns
// 9.43 rows per loop where 100 are planned, since 3 of the 53 customers have
// no event: the Limit stopped it early in the others, and EXPLAIN does not
// say in which, so it is not flagged for too few rows.
func TestExplainDoesNotFlagAScanALimitIsPlannedToStop(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	const sql = `SELECT c.id, e.id FROM (SELECT md5('customer-' || g)::uuid AS id FROM generate_series(1, 53) g) c
		CROSS JOIN LATERAL (SELECT e.id FROM events e WHERE e.payload->>'customer' = c.id::text ORDER BY e.id LIMIT 10) e`
	got := s.ok(t, "pg_explain", args("clean", sql, "analyze", true))
	if !strings.Contains(got, "\n  Index Scan using events_pkey on app.events e: ") || !strings.HasSuffix(got, "\nflags: none") {
		t.Errorf("pg_explain:\n%s", got)
	}
}

// count(*) OVER () reads the whole partition before its first row, so the
// Limit above it does not stop the seq scan under it: with analyze the scan
// is flagged, and without it too from PostgreSQL 17, whose planner costs the
// window's startup to cover the scan.
func TestExplainUnderAWindowOverTheWholePartition(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	const sql = "SELECT id, count(*) OVER () FROM events LIMIT 10"
	const flag = "\nflags:\n  Seq Scan on app.events: full scan of ~20000 rows"
	if got := s.ok(t, "pg_explain", args("clean", sql, "analyze", true)); !strings.HasSuffix(got, flag) {
		t.Errorf("analyze:\n%s", got)
	}
	version := testdb.Value[int](t, testdb.Conn(t, testdb.RoleClean, testdb.DBApp), "SELECT current_setting('server_version_num')::int")
	if version < 170000 {
		return // earlier planners give the window no startup cost that shows it
	}
	got := s.ok(t, "pg_explain", args("clean", sql))
	if !regexp.MustCompile(`\n  Seq Scan on app\.events: cost [0-9.]+, ~20000 rows\n`).MatchString(got) || !strings.HasSuffix(got, flag) {
		t.Errorf("plan only:\n%s", got)
	}
}

// A summary over the inline limit is saved to a file, one line per row: here
// 250 subqueries, each scanning app.events, which EXPLAIN tells apart by
// alias.
func TestLargeExplainSummarySpills(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	subqueries := make([]string, 250)
	for i := range subqueries {
		subqueries[i] = fmt.Sprintf("(SELECT count(*) FROM events WHERE kind = 'k%d')", i)
	}
	summary := s.ok(t, "pg_explain", args("clean", "SELECT "+strings.Join(subqueries, ", ")))
	if !strings.HasPrefix(summary, "Result too large to return inline (") || len(summary) > config.InlineMaxBytes ||
		!strings.Contains(summary, ": 258 rows × 1 column, truncated=false.\n") {
		t.Fatalf("summary (%d bytes):\n%s", len(summary), summary)
	}
	records := readSpill(t, spillPath(t, summary), s.spillDir)
	if len(records) != 259 || records[0][0] != "line" || !strings.HasPrefix(records[1][0], "total cost ") ||
		records[2][0] != "top nodes by own cost:" || records[8][0] != "flags:" {
		t.Fatalf("spill file holds %d records, starting %q", len(records), records[:min(len(records), 9)])
	}
	flag := regexp.MustCompile(`^  Seq Scan on app\.events( events_\d+)?: full scan of ~20000 rows$`)
	for _, r := range records[9:] {
		if !flag.MatchString(r[0]) {
			t.Errorf("flag %q", r[0])
		}
	}
}

// With analyze the query runs, so statement_timeout ends it; without, it
// is only planned.
func TestExplainAnalyzeIsBoundByTheStatementTimeout(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"clean": direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))})
	cfg.Defaults.StatementTimeoutSeconds = 1
	s := connect(t, cfg)
	const sql = "SELECT pg_sleep(30)"

	if got := s.ok(t, "pg_explain", args("clean", sql)); !strings.Contains(got, "\n  Result: cost ") {
		t.Errorf("plan only:\n%s", got)
	}
	if got := s.fails(t, "pg_explain", args("clean", sql, "analyze", true)); got !=
		"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)" {
		t.Errorf("analyze: %s", got)
	}
	lines := s.auditLines(t)
	if len(lines) != 2 || lines[1].Outcome != "db_error" || lines[1].SQLState != "57014" {
		t.Errorf("audit lines: %+v", lines)
	}
}

func TestExplainGoesThroughTheRoleCheck(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	got := s.fails(t, "pg_explain", args("super", "SELECT 1", "analyze", true))
	if !strings.HasPrefix(got, `connection "super" refused (role_check strict): its role can do more than read.`) ||
		!strings.Contains(got, "critical superuser") {
		t.Errorf("superuser under role_check strict:\n%s", got)
	}
	if got := s.ok(t, "pg_explain", args("clean", "SELECT 1")); !strings.HasSuffix(got, "\nflags: none") {
		t.Errorf("ro_clean under role_check strict:\n%s", got)
	}
	if lines := s.auditLines(t); len(lines) != 2 || lines[0].Outcome != "preflight_block" || lines[1].Outcome != "ok" {
		t.Errorf("audit lines: %+v", lines)
	}
}
