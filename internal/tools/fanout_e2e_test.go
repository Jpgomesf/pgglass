package tools_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// fanoutArgs are the arguments of a pg_query call on conns.
func fanoutArgs(conns []string, sql string, more ...any) map[string]any {
	a := args("", sql, more...)
	delete(a, "connection")
	a["connections"] = conns
	return a
}

// fanoutLine is the audit line of a pg_query call that ran sql on conn.
func fanoutLine(conn, sql string, rows int, truncated bool, outcome, sqlState string) auditLine {
	return auditLine{Msg: "tool call", Tool: "pg_query", Connection: conn, SQLSHA256: sha12(sql), SQLLen: len(sql),
		Rows: rows, Truncated: truncated, Outcome: outcome, SQLState: sqlState}
}

func checkFanoutAudit(t *testing.T, got, want []auditLine) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("audit lines:\n%+v\nwant\n%+v", got, want)
	}
}

// fanoutConns creates two databases whose items tables share id and name,
// the second's with a price too, and returns connections to them and to the
// app fixture:
//
//   - one and two: ro_clean on each database
//   - netted: ro_clean on the second, with default_schema net
//   - leaky and super: ro_leaky and the superuser on the app fixture
func fanoutConns(t *testing.T) map[string]config.Connection {
	t.Helper()
	const one, two = "tools_fanout_one", "tools_fanout_two"
	testdb.CreateDatabase(t, one, `CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL);
		INSERT INTO items SELECT g, 'one-' || g FROM generate_series(1, 3) g;
		GRANT SELECT ON items TO ro_clean;`)
	testdb.CreateDatabase(t, two, `CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL, price numeric NOT NULL);
		INSERT INTO items SELECT g, 'two-' || g, g * 1.5 FROM generate_series(1, 2) g;
		GRANT SELECT ON items TO ro_clean;`)
	return map[string]config.Connection{
		"one":    direct(testdb.DSN(t, testdb.RoleClean, one)),
		"two":    direct(testdb.DSN(t, testdb.RoleClean, two)),
		"netted": {Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, two), DefaultSchema: "net"},
		"leaky":  direct(testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp)),
		"super":  direct(testdb.DSN(t, testdb.Superuser, testdb.DBApp)),
	}
}

// TestFanout runs each case on its own session over fanoutConns.
func TestFanout(t *testing.T) {
	t.Parallel()
	conns := fanoutConns(t)
	for _, tc := range []struct {
		name      string
		roleCheck string
		test      func(*testing.T, *session)
	}{
		{"MergesRowsUnderAConnectionColumn", config.RoleCheckWarn, testFanoutMergesRows},
		{"AFailedConnectionIsALine", config.RoleCheckWarn, testFanoutFailedConnection},
		{"FailsWhenEveryConnectionFails", config.RoleCheckWarn, testFanoutEveryConnectionFails},
		{"ReturnsASectionPerConnectionWhenColumnsDiffer", config.RoleCheckWarn, testFanoutColumnsDiffer},
		{"AppliesTheLimitsOnEachConnection", config.RoleCheckWarn, testFanoutLimits},
		{"RoleCheckStrictRefusesConnectionsOneByOne", config.RoleCheckStrict, testFanoutRoleCheckStrict},
		{"RoleCheckWarnShowsFindingsOncePerConnection", config.RoleCheckWarn, testFanoutRoleCheckWarn},
		{"GuardsOnceAndChecksEachSearchPath", config.RoleCheckWarn, testFanoutGuard},
		{"RunsConnectionsInParallel", config.RoleCheckOff, testFanoutInParallel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.test(t, connect(t, newConfig(tc.roleCheck, conns)))
		})
	}
}

func testFanoutMergesRows(t *testing.T, s *session) {
	const sql = "SELECT id, name FROM items ORDER BY id"
	text, isErr, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"two", "one"}, sql))
	if want := "connection\tid\tname\ntwo\t1\ttwo-1\ntwo\t2\ttwo-2\none\t1\tone-1\none\t2\tone-2\none\t3\tone-3\n(5 rows)"; isErr || text != want {
		t.Errorf("tsv (error %t):\n%s\nwant\n%s", isErr, text, want)
	}
	checkFanoutAudit(t, lines, []auditLine{fanoutLine("one", sql, 3, false, "ok", ""), fanoutLine("two", sql, 2, false, "ok", "")})

	want := `{"columns":[{"name":"connection","type":"text"},{"name":"id","type":"int4"},{"name":"name","type":"text"}],` +
		`"rows":[["one",1,"one-1"],["one",2,"one-2"],["one",3,"one-3"],["two",1,"two-1"],["two",2,"two-2"]]}` + "\n(5 rows)"
	if got := s.ok(t, "pg_query", fanoutArgs([]string{"one", "two"}, sql, "format", "json")); got != want {
		t.Errorf("json:\n%s\nwant\n%s", got, want)
	}
}

func testFanoutFailedConnection(t *testing.T, s *session) {
	const sql = "SELECT id, price FROM items ORDER BY id"
	text, isErr, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"one", "two"}, sql))
	want := "connection\tid\tprice\ntwo\t1\t1.5\ntwo\t2\t3.0\n(2 rows)\n\n" +
		`one: ERROR: column "price" does not exist (SQLSTATE 42703) at character 12`
	if isErr || text != want {
		t.Errorf("one connection failed (error %t):\n%s\nwant\n%s", isErr, text, want)
	}
	checkFanoutAudit(t, lines, []auditLine{fanoutLine("one", sql, 0, false, "db_error", "42703"), fanoutLine("two", sql, 2, false, "ok", "")})
}

func testFanoutEveryConnectionFails(t *testing.T, s *session) {
	const sql = "SELECT 1/0"
	text, isErr, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"two", "one"}, sql))
	if want := "two: ERROR: division by zero (SQLSTATE 22012)\none: ERROR: division by zero (SQLSTATE 22012)"; !isErr || text != want {
		t.Errorf("every connection failed (error %t):\n%s\nwant\n%s", isErr, text, want)
	}
	checkFanoutAudit(t, lines, []auditLine{fanoutLine("one", sql, 0, false, "db_error", "22012"), fanoutLine("two", sql, 0, false, "db_error", "22012")})
}

func testFanoutColumnsDiffer(t *testing.T, s *session) {
	want := "Columns differ across connections, so each has its own result.\n\n" +
		"one:\nid\tname\n1\tone-1\n2\tone-2\n3\tone-3\n(3 rows)\n\n" +
		"two:\nid\tname\tprice\n1\ttwo-1\t1.5\n2\ttwo-2\t3.0\n(2 rows)"
	if got := s.ok(t, "pg_query", fanoutArgs([]string{"one", "two"}, "SELECT * FROM items ORDER BY id")); got != want {
		t.Errorf("columns differ:\n%s\nwant\n%s", got, want)
	}
}

func testFanoutLimits(t *testing.T, s *session) {
	const sql = "SELECT id FROM items ORDER BY id"
	text, _, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"one", "two"}, sql, "max_rows", 2))
	if want := "connection\tid\none\t1\none\t2\ntwo\t1\ntwo\t2\n(4 rows; truncated: one max_rows=2)"; text != want {
		t.Errorf("max_rows 2:\n%s\nwant\n%s", text, want)
	}
	checkFanoutAudit(t, lines, []auditLine{fanoutLine("one", sql, 2, true, "ok", ""), fanoutLine("two", sql, 2, false, "ok", "")})

	// Each connection keeps max_rows (1000) rows; together they spill.
	summary := s.ok(t, "pg_query", fanoutArgs([]string{"one", "two"}, "SELECT g FROM generate_series(1, 1500) g"))
	if len(summary) > config.InlineMaxBytes ||
		!strings.Contains(summary, "2000 rows × 2 columns, truncated=true (one max_rows=1000, two max_rows=1000).") {
		t.Fatalf("summary (%d bytes):\n%s", len(summary), summary)
	}
	path := spillPath(t, summary)
	if base := filepath.Base(path); !strings.HasPrefix(base, "spill-pg_query-one_two-") {
		t.Errorf("spill file %s does not name the connections", base)
	}
	records := readSpill(t, path, s.spillDir)
	if len(records) != 2001 || strings.Join(records[0], ",") != "connection,g" {
		t.Fatalf("%d CSV records, header %q; want connection,g and 2000 rows", len(records), records[0])
	}
	for i, want := range map[int]string{1: "one,1", 1000: "one,1000", 1001: "two,1", 2000: "two,1000"} {
		if got := strings.Join(records[i], ","); got != want {
			t.Errorf("record %d = %s, want %s", i, got, want)
		}
	}
}

func testFanoutRoleCheckStrict(t *testing.T, s *session) {
	const sql = "SELECT 1 AS n"
	text, isErr, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"one", "leaky", "super"}, sql))
	if !strings.HasPrefix(text, "connection\tn\none\t1\n(1 row)\n\n"+
		"leaky: connection \"leaky\" refused (role_check strict): its role can do more than read.\n"+
		"  role ro_leaky: 3 finding(s)\n    critical function_execute: EXECUTE on ") ||
		!strings.HasSuffix(text, "\nsuper: connection \"super\" refused (role_check strict): its role can do more than read.\n"+
			"  role postgres: 1 finding(s)\n    critical superuser: bypasses every privilege check") || isErr {
		t.Errorf("strict (error %t):\n%s", isErr, text)
	}
	checkFanoutAudit(t, lines, []auditLine{
		fanoutLine("leaky", sql, 0, false, "preflight_block", ""),
		fanoutLine("one", sql, 1, false, "ok", ""),
		fanoutLine("super", sql, 0, false, "preflight_block", ""),
	})
}

func testFanoutRoleCheckWarn(t *testing.T, s *session) {
	a := fanoutArgs([]string{"one", "leaky"}, "SELECT 1 AS n")
	const rows = "connection\tn\none\t1\nleaky\t1\n(2 rows)"
	first := s.ok(t, "pg_query", a)
	if !strings.HasPrefix(first, "leaky: role check: role ro_leaky: 3 finding(s)\n  critical function_execute: EXECUTE on ") ||
		!strings.HasSuffix(first, "\n  warning table_write: INSERT on app.audit_log\n\n"+rows) {
		t.Errorf("first result:\n%s", first)
	}
	if again := s.ok(t, "pg_query", a); again != rows {
		t.Errorf("second result repeats the findings:\n%s", again)
	}
}

func testFanoutGuard(t *testing.T, s *session) {
	const deleteSQL = "DELETE FROM items"
	text, isErr, lines := s.auditedCall(t, "pg_query", fanoutArgs([]string{"one", "two"}, deleteSQL))
	if !isErr || !strings.HasPrefix(text, "query rejected: DeleteStmt is not allowed") {
		t.Errorf("DELETE (error %t): %s", isErr, text)
	}
	checkFanoutAudit(t, lines, []auditLine{
		fanoutLine("one", deleteSQL, 0, false, "guard_reject", ""),
		fanoutLine("two", deleteSQL, 0, false, "guard_reject", ""),
	})

	// netted's default_schema is net, which the guard refuses as the
	// search_path: only netted fails.
	const sql = "SELECT count(*) AS n FROM items"
	text, isErr, lines = s.auditedCall(t, "pg_query", fanoutArgs([]string{"netted", "one"}, sql))
	if want := "connection\tn\none\t3\n(1 row)\n\n" +
		"netted: query rejected: schema net is not allowed as the search_path: its functions are denied"; isErr || text != want {
		t.Errorf("netted (error %t):\n%s\nwant\n%s", isErr, text, want)
	}
	checkFanoutAudit(t, lines, []auditLine{fanoutLine("netted", sql, 0, false, "guard_reject", ""), fanoutLine("one", sql, 1, false, "ok", "")})
}

// Run one after the other, the second query would start after the first
// ended.
func testFanoutInParallel(t *testing.T, s *session) {
	text := s.ok(t, "pg_query", fanoutArgs([]string{"one", "two"},
		"SELECT extract(epoch FROM now()) AS started, extract(epoch FROM clock_timestamp()) AS ended FROM pg_sleep(1)"))
	lines := strings.Split(text, "\n")
	if len(lines) != 4 || lines[0] != "connection\tstarted\tended" || lines[3] != "(2 rows)" {
		t.Fatalf("result:\n%s", text)
	}
	var started, ended []float64
	for _, line := range lines[1:3] {
		cells := strings.Split(line, "\t")
		start, err1 := strconv.ParseFloat(cells[1], 64)
		end, err2 := strconv.ParseFloat(cells[2], 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("row %q: %v %v", line, err1, err2)
		}
		started, ended = append(started, start), append(ended, end)
	}
	if slices.Max(started) >= slices.Min(ended) {
		t.Errorf("the queries did not overlap:\n%s", text)
	}
}

// Arguments that name no connection to run on fail with one audit line for
// no connection; from the guard on, every connection gets its own. These
// fail before any query, so they need no database.
func TestFanoutArgumentErrors(t *testing.T) {
	conns := map[string]config.Connection{}
	var eleven []string
	for i := range 11 {
		name := "c" + strconv.Itoa(i)
		conns[name] = direct(unusedDSN)
		eleven = append(eleven, name)
	}
	s := connect(t, newConfig(config.RoleCheckWarn, conns))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"connection": "c0", "connections": []string{"c1"}, "sql": "SELECT 1"}, "pass connection or connections, not both"},
		{fanoutArgs([]string{}, "SELECT 1"), "connections must list 1 to 10 names, got 0"},
		{fanoutArgs(eleven, "SELECT 1"), "connections must list 1 to 10 names, got 11"},
		{fanoutArgs([]string{"c0", "c1", "c0"}, "SELECT 1"), `connections lists "c0" twice`},
		{fanoutArgs([]string{"c0", "nope"}, "SELECT 1"), `unknown connection "nope" (available: c0, c1, c10, c2, `},
		{map[string]any{"connections": "c0", "sql": "SELECT 1"}, "invalid arguments: "},
	} {
		if got := s.fails(t, "pg_query", tc.args); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%v = %q, want it to start with %q", tc.args, got, tc.want)
		}
	}
	lines := s.auditLines(t)
	if len(lines) != 6 {
		t.Fatalf("%d audit lines, want 6:\n%s", len(lines), s.audit)
	}
	for _, line := range lines {
		if line.Connection != "" || line.Outcome != "error" {
			t.Errorf("audit line %+v: want no connection and outcome error", line)
		}
	}

	const deleteSQL = "DELETE FROM items"
	text, _, got := s.auditedCall(t, "pg_query", fanoutArgs([]string{"c1", "c0"}, deleteSQL))
	if !strings.HasPrefix(text, "query rejected: DeleteStmt is not allowed") {
		t.Errorf("DELETE: %s", text)
	}
	checkFanoutAudit(t, got, []auditLine{
		fanoutLine("c0", deleteSQL, 0, false, "guard_reject", ""),
		fanoutLine("c1", deleteSQL, 0, false, "guard_reject", ""),
	})
	text, _, got = s.auditedCall(t, "pg_query", fanoutArgs([]string{"c1", "c0"}, "SELECT 1", "format", "xml"))
	if text != `format must be one of tsv, json, markdown, got "xml"` {
		t.Errorf("format xml: %s", text)
	}
	checkFanoutAudit(t, got, []auditLine{fanoutLine("c0", "SELECT 1", 0, false, "error", ""), fanoutLine("c1", "SELECT 1", 0, false, "error", "")})
}

// pg_query takes connections in place of connection, so it requires neither.
func TestFanoutDefinition(t *testing.T) {
	tool, size := listedTool(t, "pg_query")
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var connections struct {
		Type     string            `json:"type"`
		Items    map[string]string `json:"items"`
		MaxItems int               `json:"maxItems"`
	}
	if err := json.Unmarshal(schema.Properties["connections"], &connections); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Required, []string{"sql"}) || connections.Type != "array" ||
		connections.Items["type"] != "string" || connections.MaxItems != 10 {
		t.Errorf("pg_query input schema: %s", data)
	}
	t.Logf("pg_query definition: %d bytes of JSON, %d of them the connections argument", size, len(schema.Properties["connections"]))
}
