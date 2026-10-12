package tools_test

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/guard/guardtest"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// appConns connects to the app fixture as ro_clean (with default_schema
// app), ro_leaky and the superuser.
func appConns(t *testing.T) map[string]config.Connection {
	t.Helper()
	return map[string]config.Connection{
		"clean": {Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, testdb.DBApp), DefaultSchema: "app"},
		"leaky": direct(testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp)),
		"super": direct(testdb.DSN(t, testdb.Superuser, testdb.DBApp)),
	}
}

func observe(t *testing.T, conn *pgx.Conn, sql string) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(t.Context(), sql).Scan(&s); err != nil {
		t.Fatalf("observe %q: %v", sql, err)
	}
	return s
}

func args(connection, sql string, more ...any) map[string]any {
	a := map[string]any{"connection": connection, "sql": sql}
	for i := 0; i+1 < len(more); i += 2 {
		a[more[i].(string)] = more[i+1]
	}
	return a
}

func TestQueryReturnsPostgresTextInEveryFormat(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	const sql = `SELECT 1 AS n, 'a b' AS s, NULL::text AS z, true AS b, '{"k": [1, 2]}'::jsonb AS j,
		12.30::numeric(12, 2) AS m, '0ddc4d69-0dcf-fba9-15c2-88a68e6665de'::uuid AS u, 2 AS n`
	tsv := "n\ts\tz\tb\tj\tm\tu\tn\n" +
		"1\ta b\t\\N\tt\t{\"k\": [1, 2]}\t12.30\t0ddc4d69-0dcf-fba9-15c2-88a68e6665de\t2\n(1 row)"
	for format, want := range map[string]string{
		"":    tsv, // the configured default
		"tsv": tsv,
		"json": `{"columns":[{"name":"n","type":"int4"},{"name":"s","type":"text"},{"name":"z","type":"text"},` +
			`{"name":"b","type":"bool"},{"name":"j","type":"jsonb"},{"name":"m","type":"numeric"},` +
			`{"name":"u","type":"uuid"},{"name":"n","type":"int4"}],` +
			`"rows":[[1,"a b",null,true,{"k":[1,2]},12.30,"0ddc4d69-0dcf-fba9-15c2-88a68e6665de",2]]}` + "\n(1 row)",
		"markdown": "| n | s | z | b | j | m | u | n |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n" +
			"| 1 | a b | NULL | t | {\"k\": [1, 2]} | 12.30 | 0ddc4d69-0dcf-fba9-15c2-88a68e6665de | 2 |\n(1 row)",
	} {
		a := args("clean", sql)
		if format != "" {
			a["format"] = format
		}
		if got := s.ok(t, "pg_query", a); got != want {
			t.Errorf("format %q:\n%s\nwant\n%s", format, got, want)
		}
	}
}

func TestQueryArguments(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"params", args("clean", "SELECT $1::int + 1 AS n, $2::text AS t, $3::text IS NULL AS null_param, $4::bool AS b",
			"params", []any{41, "x'); DROP TABLE app.customers; --", nil, true}),
			"n\tt\tnull_param\tb\n42\tx'); DROP TABLE app.customers; --\tt\tt\n(1 row)"},
		{"exact numbers", args("clean", "SELECT $1::numeric AS big, $2::bigint AS id, $3::float8 AS f",
			"params", []any{json.Number("12345678901234567890.123"), json.Number("9007199254740993"), 0.1}),
			"big\tid\tf\n12345678901234567890.123\t9007199254740993\t0.1\n(1 row)"},
		{"default_schema", args("clean", "SELECT count(*) FROM customers"), "count\n50\n(1 row)"},
		{"schema", args("clean", "SELECT count(*) FROM orders", "schema", "app"), "count\n50\n(1 row)"},
		{"max_rows", args("clean", "SELECT id FROM events ORDER BY id", "max_rows", 3),
			"id\n1\n2\n3\n(3 rows; truncated: max_rows=3)"},
		{"no rows", args("clean", "SELECT id FROM events WHERE false"), "id\n(0 rows)"},
		{"EXPLAIN", args("clean", "EXPLAIN (COSTS OFF) SELECT 1"), "QUERY PLAN\nResult\n(1 row)"},
		{"SHOW", args("clean", "SHOW transaction_read_only"), "transaction_read_only\non\n(1 row)"},
	} {
		if got := s.ok(t, "pg_query", tc.args); got != tc.want {
			t.Errorf("%s:\n%s\nwant\n%s", tc.name, got, tc.want)
		}
	}

	// max_rows only lowers the configured cap of 1000.
	got := s.ok(t, "pg_query", args("clean", "SELECT g FROM generate_series(1, 2000) g", "max_rows", 5000))
	if !strings.HasSuffix(got, "\n1000\n(1000 rows; truncated: max_rows=1000)") {
		t.Errorf("max_rows above the cap: ends with %q", got[max(0, len(got)-60):])
	}

	text := s.fails(t, "pg_query", args("clean", "SELECT $1::int[]", "params", []any{[]any{1, 2}}))
	if !strings.HasPrefix(text, "params[1] is an array or object") {
		t.Errorf("array param: %s", text)
	}

	// schema replaces default_schema, so customers is not found in public.
	text = s.fails(t, "pg_query", args("clean", "SELECT count(*) FROM customers", "schema", "public"))
	if !strings.Contains(text, "(SQLSTATE 42P01)") {
		t.Errorf("customers in public: %s", text)
	}
}

func TestDatabaseErrorsAreToolErrors(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{args("clean", "SELECT nam FROM customers"), `ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8` + "\n" +
			`HINT: Perhaps you meant to reference the column "customers.name".`},
		{args("clean", "SELECT 'x'::jsonb"), "ERROR: invalid input syntax for type json (SQLSTATE 22P02) at character 8\n" +
			`DETAIL: Token "x" is invalid.`},
		{args("clean", "SELECT 1/0"), "ERROR: division by zero (SQLSTATE 22012)"},
		{args("clean", "SELECT 1", "params", []any{1}), "(SQLSTATE 08P01)"}, // a parameter the query has no place for
		{args("clean", "SELECT id FROM tenant_notes FOR UPDATE"), "query rejected: FOR UPDATE/FOR SHARE is not allowed"},
	} {
		if got := s.fails(t, "pg_query", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%v:\n%s\nwant it to contain\n%s", tc.args, got, tc.want)
		}
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pg_schema", map[string]any{"connection": "clean", "schema": "nope"}, `schema "nope" does not exist or the current role lacks USAGE on it`},
		{"pg_schema", map[string]any{"connection": "clean", "schema": "app", "table": "nope"}, `relation "app"."nope" does not exist`},
	} {
		if got := s.fails(t, tc.tool, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s %v = %q, want it to contain %q", tc.tool, tc.args, got, tc.want)
		}
	}
	// The connection is still usable.
	if got := s.ok(t, "pg_query", args("clean", "SELECT 1 AS one")); got != "one\n1\n(1 row)" {
		t.Errorf("after the errors: %q", got)
	}
}

// sizes matches the pretty sizes in pg_schema output, which vary by version.
var sizes = regexp.MustCompile(`\d+ (bytes|kB|MB)`)

func TestSchemaDescribesStepByStep(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	schema := func(a map[string]any) string {
		t.Helper()
		a["connection"] = "clean"
		return sizes.ReplaceAllString(s.ok(t, "pg_schema", a), "SIZE")
	}
	if got, want := schema(map[string]any{}),
		"app: 7 relations -- Fixture application schema\npublic: 0 relations -- standard public schema"; got != want {
		t.Errorf("schemas:\n%s\nwant\n%s", got, want)
	}
	if got, want := schema(map[string]any{"schema": "app"}), `app.audit_log: table, ~50 rows, SIZE
app.customer_order_totals: view
app.customers: table, ~50 rows, SIZE -- People who place orders
app.daily_event_counts: materialized view, ~56 rows, SIZE
app.events: table, ~20000 rows, SIZE
app.orders: table, ~50 rows, SIZE
app.tenant_notes: table, ~50 rows, SIZE, RLS`; got != want {
		t.Errorf("relations of app:\n%s\nwant\n%s", got, want)
	}
	orders := `app.orders: table, ~50 rows, SIZE
  id bigint NOT NULL GENERATED ALWAYS AS IDENTITY
  customer_id uuid NOT NULL
  status app.order_status NOT NULL DEFAULT 'pending'::app.order_status -- enum: 'pending', 'paid', 'shipped', 'cancelled'
  total numeric(12,2) NOT NULL
  quantities integer[] NOT NULL
  metadata jsonb
  fulfillment interval
  receipt bytea
  placed_at timestamp with time zone NOT NULL
constraints:
  orders_pkey PRIMARY KEY (id)
  orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES app.customers(id)
  orders_total_check CHECK (total >= 0::numeric)
indexes:
  CREATE INDEX orders_customer_id_idx ON app.orders USING btree (customer_id)
  CREATE INDEX orders_pending_placed_at_idx ON app.orders USING btree (placed_at) WHERE status = 'pending'::app.order_status`
	if got := schema(map[string]any{"schema": "app", "table": "orders"}); got != orders {
		t.Errorf("app.orders:\n%s\nwant\n%s", got, orders)
	}
	// Without schema, table is looked up in the connection's default_schema.
	if got := schema(map[string]any{"table": "orders"}); got != orders {
		t.Errorf("orders in default_schema app:\n%s", got)
	}
}

func TestSchemaIsCached(t *testing.T) {
	t.Parallel()
	const name = "tools_cache"
	testdb.CreateDatabase(t, name, "CREATE TABLE before_listing (id int)")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))}))
	list := map[string]any{"connection": "db", "schema": "public"}
	first := s.ok(t, "pg_schema", list)
	if !strings.Contains(first, "public.before_listing: table") {
		t.Fatalf("first listing:\n%s", first)
	}

	if _, err := testdb.Conn(t, testdb.Superuser, name).Exec(t.Context(), "CREATE TABLE after_listing (id int)"); err != nil {
		t.Fatal(err)
	}
	if again := s.ok(t, "pg_schema", list); again != first {
		t.Errorf("second listing changed within the cache TTL:\n%s", again)
	}
	// pg_search is not cached: the new table is there.
	if got := s.ok(t, "pg_search", map[string]any{"connection": "db", "pattern": "after_listing"}); got != "public.after_listing: table" {
		t.Errorf("pg_search after_listing = %q", got)
	}
}

// A description longer than the inline limit is cut at a line, with a note.
func TestSchemaOutputIsCutToTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_many_tables"
	testdb.CreateDatabase(t, name, `DO $$ BEGIN FOR i IN 1..500 LOOP
		EXECUTE format('CREATE TABLE t%s (id int)', lpad(i::text, 3, '0'));
	END LOOP; END $$`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))}))

	got := s.ok(t, "pg_schema", map[string]any{"connection": "db", "schema": "public"})
	lines := strings.Split(got, "\n")
	note := fmt.Sprintf("… %d of 500 lines shown (output limit %d bytes): use pg_search to find names", len(lines)-1, config.InlineMaxBytes)
	if len(got) > config.InlineMaxBytes || len(lines) < 100 || !strings.HasPrefix(got, "public.t001: table") || lines[len(lines)-1] != note {
		t.Errorf("pg_schema of 500 tables (%d bytes):\n%s", len(got), got)
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "ORDER"}, "app.customer_order_totals: view\napp.orders: table\napp.customer_order_totals.order_count: bigint"},
		{map[string]any{"pattern": "order", "limit": 2}, "app.customer_order_totals: view\napp.orders: table"},
		{map[string]any{"pattern": "%"}, "no matches"},
	} {
		tc.args["connection"] = "clean"
		if got := s.ok(t, "pg_search", tc.args); got != tc.want {
			t.Errorf("pg_search %v:\n%s\nwant\n%s", tc.args, got, tc.want)
		}
	}
}

func TestListConnectionsShowsRoleChecks(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	for _, conn := range []string{"clean", "leaky", "super"} {
		s.ok(t, "pg_query", args(conn, "SELECT 1"))
	}
	got := s.ok(t, "pg_list_connections", nil)
	for _, want := range []string{
		"Connections (3):\n  clean [direct, schema:app]\n    role ro_clean: no findings\n  leaky [direct]\n    role ro_leaky: 3 finding(s)\n      critical function_execute: EXECUTE on ",
		"\n      warning role_membership: member of pg_read_server_files\n      warning table_write: INSERT on app.audit_log\n" +
			"  super [direct]\n    role postgres: 1 finding(s)\n      critical superuser: bypasses every privilege check",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pg_list_connections:\n%s\nwant it to contain\n%s", got, want)
		}
	}
	if strings.Contains(got, "refused") {
		t.Errorf("role_check warn refuses nothing:\n%s", got)
	}
}

// The superuser and ro_leaky, which may EXECUTE dblink, have critical
// findings. Warnings alone, such as ro_leaky's INSERT, would not be refused.
func TestRoleCheckStrictRefusesRolesWithCriticalFindings(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pg_query", args("super", "SELECT 1"), "critical superuser: bypasses every privilege check"},
		{"pg_query", args("leaky", "SELECT 1"), "critical function_execute: EXECUTE on "},
		{"pg_schema", map[string]any{"connection": "super"}, "critical superuser"},
		{"pg_search", map[string]any{"connection": "leaky", "pattern": "x"}, "critical function_execute"},
	} {
		got := s.fails(t, tc.tool, tc.args)
		prefix := fmt.Sprintf("connection %q refused (role_check strict): its role can do more than read.\n", tc.args["connection"])
		if !strings.HasPrefix(got, prefix) || !strings.Contains(got, tc.want) {
			t.Errorf("%s %v:\n%s", tc.tool, tc.args, got)
		}
	}
	if got := s.ok(t, "pg_query", args("clean", "SELECT 1 AS one")); got != "one\n1\n(1 row)" {
		t.Errorf("ro_clean under role_check strict: %q", got)
	}

	list := s.ok(t, "pg_list_connections", nil)
	for _, want := range []string{
		"  clean [direct, schema:app]\n    role ro_clean: no findings\n",
		"      warning table_write: INSERT on app.audit_log\n    refused: role_check is strict\n",
		"      critical superuser: bypasses every privilege check\n    refused: role_check is strict",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("pg_list_connections:\n%s\nwant it to contain\n%s", list, want)
		}
	}
	for _, line := range s.auditLines(t) {
		want := "preflight_block"
		if line.Connection == "clean" || line.Tool == "pg_list_connections" {
			want = "ok"
		}
		if line.Outcome != want {
			t.Errorf("audit line %+v: want outcome %s", line, want)
		}
	}
}

func TestRoleCheckWarnShowsFindingsOnce(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))

	first := s.ok(t, "pg_query", args("leaky", "SELECT 1 AS one"))
	if !strings.HasPrefix(first, "role check: role ro_leaky: 3 finding(s)\n  critical function_execute: EXECUTE on ") ||
		!strings.HasSuffix(first, "\n  warning role_membership: member of pg_read_server_files\n"+
			"  warning table_write: INSERT on app.audit_log\n\none\n1\n(1 row)") {
		t.Errorf("first result on ro_leaky:\n%s", first)
	}
	if again := s.ok(t, "pg_query", args("leaky", "SELECT 1 AS one")); again != "one\n1\n(1 row)" {
		t.Errorf("second result on ro_leaky repeats the findings:\n%s", again)
	}

	// The findings come with the first result even when it is an error.
	failed := s.fails(t, "pg_query", args("super", "SELECT 1/0"))
	if want := "role check: role postgres: 1 finding(s)\n  critical superuser: bypasses every privilege check\n\n" +
		"ERROR: division by zero (SQLSTATE 22012)"; failed != want {
		t.Errorf("first result on the superuser:\n%s\nwant\n%s", failed, want)
	}

	if got := s.ok(t, "pg_schema", map[string]any{"connection": "clean"}); strings.Contains(got, "role check") {
		t.Errorf("ro_clean has no findings, yet:\n%s", got)
	}
}

func TestRoleCheckOffSkipsTheAudit(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, appConns(t)))
	if got := s.ok(t, "pg_query", args("super", "SELECT 1 AS one")); got != "one\n1\n(1 row)" {
		t.Errorf("superuser under role_check off: %q", got)
	}
	if got := s.ok(t, "pg_list_connections", nil); strings.Count(got, "role check off") != 3 {
		t.Errorf("pg_list_connections:\n%s", got)
	}
}

func TestParallelQueriesOnOneConnection(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: args("clean",
				"SELECT $1::int AS i, count(*) AS n, pg_sleep(0.05) FROM events WHERE id % 20 = $1", "params", []any{i})})
			if err != nil {
				t.Errorf("query %d: %v", i, err)
				return
			}
			var text string // not resultText: t.Fatal must not run on this goroutine
			if len(res.Content) == 1 {
				if c, ok := res.Content[0].(*mcp.TextContent); ok {
					text = c.Text
				}
			}
			if want := fmt.Sprintf("i\tn\tpg_sleep\n%d\t1000\t", i); res.IsError || !strings.HasPrefix(text, want) {
				t.Errorf("query %d:\n%s\nwant it to start with\n%s", i, text, want)
			}
		}()
	}
	wg.Wait()
	ok := 0
	for _, line := range s.auditLines(t) {
		if line.Tool == "pg_query" && line.Outcome == "ok" && line.Rows == 1 {
			ok++
		}
	}
	if ok != 20 {
		t.Errorf("%d ok audit lines, want 20:\n%s", ok, s.audit)
	}
}

// readSpill reads the spill file at path, which must be a private file in
// dir, as CSV.
func readSpill(t *testing.T, path, dir string) [][]string {
	t.Helper()
	if filepath.Dir(path) != dir {
		t.Errorf("spill file %s is not in the spill directory %s", path, dir)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("spill file mode = %v, want 0600", mode)
	}
	f, err := os.Open(path) //nolint:gosec // the path the server returned, in a test directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// spillPath returns the file a spill summary points at.
func spillPath(t *testing.T, summary string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^Full result: (.+)$`).FindStringSubmatch(summary)
	if m == nil {
		t.Fatalf("no spill file in:\n%s", summary)
	}
	return m[1]
}

func TestLargeResultSpillsToCSV(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, appConns(t))
	cfg.Defaults.MaxRows, cfg.Defaults.MaxBytes = 50_000, 16<<20
	s := connect(t, cfg)

	summary := s.ok(t, "pg_query", args("clean", "SELECT id, kind, payload, occurred_at FROM events ORDER BY id"))
	if len(summary) > config.InlineMaxBytes || !strings.Contains(summary, "20000 rows × 4 columns, truncated=false.") {
		t.Fatalf("summary (%d bytes):\n%s", len(summary), summary)
	}
	records := readSpill(t, spillPath(t, summary), s.spillDir)
	if len(records) != 20_001 {
		t.Fatalf("%d CSV records, want a header and 20000 rows", len(records))
	}
	if got := strings.Join(records[0], ","); got != "id,kind,payload,occurred_at" {
		t.Errorf("header = %s", got)
	}
	if first := records[1]; first[0] != "1" || first[1] != "click" || !strings.HasPrefix(first[2], `{"seq": 1, `) ||
		first[3] != "2024-01-01 00:01:00+00" {
		t.Errorf("first row = %q", first)
	}
	if last := records[20_000]; last[0] != "20000" || last[1] != "view" {
		t.Errorf("last row = %q", last)
	}

	// With the default max_rows, the summary says the result was cut.
	capped := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	summary = capped.ok(t, "pg_query", args("clean", "SELECT * FROM events ORDER BY id"))
	if !strings.Contains(summary, "1000 rows × 4 columns, truncated=true (max_rows=1000).") {
		t.Errorf("summary of a truncated result:\n%s", summary)
	}
}

func TestAuditLinesHoldNoSQLOrValues(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, appConns(t)))
	const (
		okSQL     = "SELECT $1::text AS marked_value"
		failSQL   = "SELECT 1/0 AS zebra_column"
		truncSQL  = "SELECT id FROM events"
		markedArg = "zebra-marker-99"
	)
	if got := s.ok(t, "pg_query", args("clean", okSQL, "params", []any{markedArg})); !strings.Contains(got, markedArg) {
		t.Fatalf("result = %q", got)
	}
	s.fails(t, "pg_query", args("clean", failSQL))
	s.ok(t, "pg_query", args("clean", truncSQL, "max_rows", 2))
	s.ok(t, "pg_search", map[string]any{"connection": "clean", "pattern": "zebra-pattern"})
	s.ok(t, "pg_schema", map[string]any{"connection": "clean", "schema": "app"})

	want := []auditLine{
		{Msg: "tool call", Tool: "pg_query", Connection: "clean", SQLSHA256: sha12(okSQL), SQLLen: len(okSQL), Rows: 1, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_query", Connection: "clean", SQLSHA256: sha12(failSQL), SQLLen: len(failSQL), Outcome: "db_error", SQLState: "22012"},
		{Msg: "tool call", Tool: "pg_query", Connection: "clean", SQLSHA256: sha12(truncSQL), SQLLen: len(truncSQL), Rows: 2, Truncated: true, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_search", Connection: "clean", Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_schema", Connection: "clean", Outcome: "ok"},
	}
	lines := s.auditLines(t)
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(want), s.audit)
	}
	for i, line := range lines {
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("line %d = %+v\nwant     %+v", i, line, want[i])
		}
	}
	for _, leak := range []string{markedArg, "zebra", "SELECT", "marked_value", "events"} {
		if strings.Contains(s.audit.String(), leak) {
			t.Errorf("audit log contains %q:\n%s", leak, s.audit)
		}
	}
}

// stateSQL fingerprints what the bypass corpus tries to change in a
// database, and in the cluster around it.
const stateSQL = `SELECT concat_ws(' ',
	'relations=' || (SELECT count(*) FROM pg_class),
	'acl=' || (SELECT md5(coalesce(string_agg(relacl::text, ',' ORDER BY oid), '')) FROM pg_class),
	'functions=' || (SELECT count(*) FROM pg_proc),
	'comments=' || (SELECT count(*) FROM pg_description),
	'extensions=' || (SELECT count(*) FROM pg_extension),
	'sequences=' || (SELECT coalesce(string_agg(format('%s.%s:%s', schemaname, sequencename, last_value), ','
		ORDER BY schemaname, sequencename), '') FROM pg_sequences),
	'analyzed=' || (SELECT coalesce(string_agg(format('%s:%s:%s', relname, last_analyze, last_vacuum), ',' ORDER BY relid), '')
		FROM pg_stat_user_tables),
	'large_objects=' || (SELECT count(*) FROM pg_largeobject_metadata),
	'roles=' || (SELECT count(*) FROM pg_roles),
	'role_settings=' || (SELECT count(*) FROM pg_db_role_setting
		WHERE setdatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))),
	'slots=' || (SELECT count(*) FROM pg_replication_slots),
	'origins=' || (SELECT count(*) FROM pg_replication_origin),
	'advisory_locks=' || (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'),
	'stats_reset=' || coalesce((SELECT stats_reset::text FROM pg_stat_database WHERE datname = current_database()), 'never'),
	'auto_conf=' || md5(pg_read_file('postgresql.auto.conf')),
	'files=' || (SELECT string_agg(f || ':' || (pg_stat_file(f, true) IS NOT NULL), ',' ORDER BY f)
		FROM unnest(ARRAY['/tmp/pwned', '/tmp/dbhub_poc', '/tmp/orders.csv']) AS f))`

const appRowsSQL = `SELECT format('customers=%s orders=%s events=%s notes=%s audit=%s daily=%s',
	(SELECT count(*) FROM app.customers), (SELECT count(*) FROM app.orders), (SELECT count(*) FROM app.events),
	(SELECT count(*) FROM app.tenant_notes), (SELECT count(*) FROM app.audit_log), (SELECT count(*) FROM app.daily_event_counts))`

const supabaseRowsSQL = `SELECT format('queued=%s profiles=%s',
	(SELECT count(*) FROM net.http_request_queue), (SELECT count(*) FROM public.profiles))`

// The whole bypass corpus, sent as the superuser, the worst case: the guard
// rejects every statement, and nothing changes in either database.
func TestBypassCorpusIsRejectedAndChangesNothing(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"app":      direct(testdb.DSN(t, testdb.Superuser, testdb.DBApp)),
		"supabase": direct(testdb.DSN(t, testdb.Superuser, testdb.DBSupabase)),
	}))
	app, supabase := testdb.Conn(t, testdb.Superuser, testdb.DBApp), testdb.Conn(t, testdb.Superuser, testdb.DBSupabase)
	state := func() string {
		return observe(t, app, stateSQL) + "\n" + observe(t, app, appRowsSQL) + "\n" +
			observe(t, supabase, stateSQL) + "\n" + observe(t, supabase, supabaseRowsSQL)
	}
	before := state()
	if !strings.Contains(before, "queued=0 ") || !strings.Contains(before, "events=20000 ") {
		t.Fatalf("unexpected starting state:\n%s", before)
	}

	for _, conn := range []string{"app", "supabase"} {
		for _, c := range guardtest.Rejected {
			got := s.fails(t, "pg_query", args(conn, c.SQL))
			if !strings.HasPrefix(got, "query rejected: ") || !strings.Contains(got, c.Want) {
				t.Errorf("%s: %q\n= %q, want a rejection naming %q", conn, c.SQL, got, c.Want)
			}
		}
	}

	// The sessions are intact and still read-only.
	for _, conn := range []string{"app", "supabase"} {
		got := s.ok(t, "pg_query", args(conn, `SELECT current_user, current_setting('default_transaction_read_only') AS dtro,
			current_setting('transaction_read_only') AS tro`))
		if !strings.HasSuffix(got, "current_user\tdtro\ttro\npostgres\ton\ton\n(1 row)") {
			t.Errorf("%s after the corpus:\n%s", conn, got)
		}
	}
	if after := state(); after != before {
		t.Errorf("state changed:\nbefore %s\nafter  %s", before, after)
	}
	rejected := 0
	for _, line := range s.auditLines(t) {
		if line.Outcome == "guard_reject" {
			rejected++
		}
	}
	if want := 2 * len(guardtest.Rejected); rejected != want {
		t.Errorf("%d guard_reject audit lines, want %d", rejected, want)
	}
}

// The statement below is one string literal when standard_conforming_strings
// is on, as the guard reads it. With the setting off, the backslash escapes
// the quote after it, the literal ends early, and the dblink_exec call after
// it would run. The engine turns the setting on, so Postgres reads the SQL as
// the guard did.
func TestBackslashCannotHideACallFromTheGuard(t *testing.T) {
	t.Parallel()
	const name = "tools_scs_off"
	testdb.CreateDatabase(t, name, "CREATE EXTENSION dblink; CREATE TABLE t (v int);"+
		"ALTER DATABASE "+name+" SET standard_conforming_strings = off;")
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))}))

	const smuggled = `SELECT 'x\'', dblink_exec($$dbname=` + name + `$$, $$INSERT INTO t VALUES (1)$$) --' AS v`
	got := s.ok(t, "pg_query", args("db", smuggled))
	if want := `v` + "\n" + `x\\', dblink_exec($$dbname=` + name + `$$, $$INSERT INTO t VALUES (1)$$) --` + "\n(1 row)"; !strings.HasSuffix(got, want) {
		t.Errorf("result:\n%s\nwant it to end with\n%s", got, want)
	}
	if n := observe(t, testdb.Conn(t, testdb.Superuser, name), "SELECT count(*)::text FROM t"); n != "0" {
		t.Errorf("t has %s rows: the hidden dblink_exec ran", n)
	}
}
