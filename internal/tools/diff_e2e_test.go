package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// The two sides of a schema diff. In public, staging changes items in each
// way pg_diff_schema compares, has a table prod lacks and lacks one prod
// has. Its schema extra has one table more than prod's, and it lacks prod's
// schema archive.
const (
	diffProdSQL = `CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL, price numeric(10, 2) CHECK (price >= 0));
		CREATE VIEW item_names AS SELECT id, name FROM items;
		CREATE TABLE legacy (id int);
		CREATE SCHEMA extra;
		CREATE SCHEMA archive;
		GRANT USAGE ON SCHEMA extra, archive TO ro_clean;
		GRANT SELECT ON ALL TABLES IN SCHEMA public TO ro_clean;`
	diffStagingSQL = `CREATE TABLE items (id bigint PRIMARY KEY, name text, price numeric(10, 2) CHECK (price > 0),
			added_at timestamptz DEFAULT now());
		CREATE INDEX items_name_idx ON items (name);
		ALTER TABLE items ENABLE ROW LEVEL SECURITY;
		CREATE POLICY items_read ON items FOR SELECT USING (true);
		CREATE VIEW item_names AS SELECT id, name FROM items;
		CREATE TABLE fresh (id int);
		CREATE SCHEMA extra;
		CREATE TABLE extra.added (id int);
		GRANT USAGE ON SCHEMA extra TO ro_clean;
		GRANT SELECT ON ALL TABLES IN SCHEMA public, extra TO ro_clean;`
)

func diffArgs(left, right string, more ...any) map[string]any {
	a := map[string]any{"left": left, "right": right}
	for i := 0; i+1 < len(more); i += 2 {
		a[more[i].(string)] = more[i+1]
	}
	return a
}

func TestDiffSchemaComparesTwoConnections(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_diff_prod", diffProdSQL)
	testdb.CreateDatabase(t, "tools_diff_staging", diffStagingSQL)
	prod := direct(testdb.DSN(t, testdb.RoleClean, "tools_diff_prod"))
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"prod":       prod,
		"prod_again": prod,
		"staging":    direct(testdb.DSN(t, testdb.RoleClean, "tools_diff_staging")),
	}))

	// Without schema, public is compared.
	want := "schema public, prod → staging: 10 differences (- only in prod, + only in staging)\n" +
		"+ table fresh\n" +
		"~ column item_names.id: integer → bigint\n" +
		"~ relation items: table → table with RLS\n" +
		"+ column items.added_at\n" +
		"~ column items.id: integer NOT NULL → bigint NOT NULL\n" +
		"~ column items.name: text NOT NULL → text\n" +
		"~ constraint items.items_price_check: CHECK (price >= 0::numeric) → CHECK (price > 0::numeric)\n" +
		"+ index items.items_name_idx\n" +
		"+ policy items.items_read\n" +
		"- table legacy"
	if got := s.ok(t, "pg_diff_schema", diffArgs("prod", "staging")); got != want {
		t.Errorf("prod against staging:\n%s\nwant\n%s", got, want)
	}
	if got, want := s.ok(t, "pg_diff_schema", diffArgs("prod", "prod_again", "schema", "public")),
		"schema public, prod → prod_again: identical (3 relations)"; got != want {
		t.Errorf("prod against itself = %q, want %q", got, want)
	}
	if got, want := s.ok(t, "pg_diff_schema", diffArgs("prod", "staging", "schema", "extra")),
		"schema extra, prod → staging: 1 difference (- only in prod, + only in staging)\n+ table added"; got != want {
		t.Errorf("schema extra = %q, want %q", got, want)
	}
	if got, want := s.ok(t, "pg_diff_schema", diffArgs("prod", "prod_again", "schema", "archive")),
		"schema archive, prod → prod_again: identical (0 relations)"; got != want {
		t.Errorf("an empty schema = %q, want %q", got, want)
	}
	// The error names the side that lacks the schema.
	if got, want := s.fails(t, "pg_diff_schema", diffArgs("prod", "staging", "schema", "archive")),
		`staging: schema "archive" does not exist or the current role lacks USAGE on it`; !strings.HasSuffix(got, want) {
		t.Errorf("a schema staging lacks = %q, want it to end with %q", got, want)
	}

	wantLines := []auditLine{
		{Msg: "tool call", Tool: "pg_diff_schema", Connection: "prod,staging", Rows: 10, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_diff_schema", Connection: "prod,prod_again", Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_diff_schema", Connection: "prod,staging", Rows: 1, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_diff_schema", Connection: "prod,prod_again", Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_diff_schema", Connection: "prod,staging", Outcome: "db_error"},
	}
	lines := s.auditLines(t)
	if len(lines) != len(wantLines) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(wantLines), s.audit)
	}
	for i, line := range lines {
		line.DurationMS = nil
		if line != wantLines[i] {
			t.Errorf("audit line %d = %+v\nwant %+v", i, line, wantLines[i])
		}
	}
}

// Views and materialized views whose columns match but whose queries differ
// take one line each, with both queries.
func TestDiffSchemaComparesViewQueries(t *testing.T) {
	t.Parallel()
	// Queries that read no table deparse alike on every supported server.
	testdb.CreateDatabase(t, "tools_diff_views_old", `CREATE VIEW answer AS SELECT 41 AS n;
		CREATE MATERIALIZED VIEW answers AS SELECT 41 AS n;`)
	testdb.CreateDatabase(t, "tools_diff_views_new", `CREATE VIEW answer AS SELECT 42 AS n;
		CREATE MATERIALIZED VIEW answers AS SELECT 42 AS n;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"old": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_views_old")),
		"new": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_views_new")),
	}))

	want := "schema public, old → new: 2 differences (- only in old, + only in new)\n" +
		"~ view answer: SELECT 41 AS n; → SELECT 42 AS n;\n" +
		"~ materialized view answers: SELECT 41 AS n; → SELECT 42 AS n;"
	if got := s.ok(t, "pg_diff_schema", diffArgs("old", "new")); got != want {
		t.Errorf("old against new:\n%s\nwant\n%s", got, want)
	}
}

// Each side's structure is one catalog result, which max_bytes bounds: a
// schema too large for it fails, naming its side and the setting to raise.
func TestDiffSchemaNamesTheSideOverMaxBytes(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_diff_small", "CREATE TABLE one (id int)")
	testdb.CreateDatabase(t, "tools_diff_large", `DO $$ BEGIN FOR i IN 1..20 LOOP
		EXECUTE format('CREATE TABLE table_%s (id int)', i);
	END LOOP; END $$`)
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{
		"small": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_small")),
		"large": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_large")),
	})
	cfg.Defaults.MaxBytes = 1000 // small's structure fits, large's does not
	s := connect(t, cfg)

	const want = `large: read the structure of schema "public": catalog result exceeds the configured limits (max_bytes=1000); ` +
		"raise defaults.max_bytes in config.json to read larger schemas"
	for _, args := range []map[string]any{diffArgs("small", "large"), diffArgs("large", "small")} {
		if got := s.fails(t, "pg_diff_schema", args); got != want {
			t.Errorf("%v:\n%s\nwant\n%s", args, got, want)
		}
	}
	for _, line := range s.auditLines(t) {
		if line.Outcome != "db_error" || line.SQLState != "" {
			t.Errorf("audit line %+v, want a db_error with no SQLSTATE", line)
		}
	}
}

// A Postgres error on one side starts with that side's name too.
func TestDiffSchemaNamesTheSideOfAPostgresError(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_diff_fine", "CREATE TABLE items (id int PRIMARY KEY)")
	// The catalog query calls pg_get_constraintdef, which ro_clean may not call there.
	testdb.CreateDatabase(t, "tools_diff_broken", `CREATE TABLE items (id int PRIMARY KEY);
		REVOKE EXECUTE ON FUNCTION pg_catalog.pg_get_constraintdef(oid, boolean) FROM PUBLIC;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"fine":   direct(testdb.DSN(t, testdb.RoleClean, "tools_diff_fine")),
		"broken": direct(testdb.DSN(t, testdb.RoleClean, "tools_diff_broken")),
	}))

	const want = "broken: ERROR: permission denied for function pg_get_constraintdef (SQLSTATE 42501)"
	for _, args := range []map[string]any{diffArgs("fine", "broken"), diffArgs("broken", "fine")} {
		if got := s.fails(t, "pg_diff_schema", args); got != want {
			t.Errorf("%v:\n%s\nwant\n%s", args, got, want)
		}
	}
	for _, line := range s.auditLines(t) {
		if line.Outcome != "db_error" || line.SQLState != "42501" {
			t.Errorf("audit line %+v, want a db_error with SQLSTATE 42501", line)
		}
	}
}

// Both connections go through the role check: under warn, findings start
// the result under the connection's name; under strict, either side with a
// critical finding refuses the call.
func TestDiffSchemaChecksBothRoles(t *testing.T) {
	t.Parallel()
	warn := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	const identical = "schema app, clean → super: identical (7 relations)"
	got := warn.ok(t, "pg_diff_schema", diffArgs("clean", "super", "schema", "app"))
	if want := "super: role check: role postgres: 1 finding(s)\n  critical superuser: bypasses every privilege check\n\n" +
		identical; got != want {
		t.Errorf("first call:\n%s\nwant\n%s", got, want)
	}
	if again := warn.ok(t, "pg_diff_schema", diffArgs("clean", "super", "schema", "app")); again != identical {
		t.Errorf("second call repeats the findings:\n%s", again)
	}

	strict := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	for _, a := range []map[string]any{diffArgs("clean", "super"), diffArgs("super", "clean")} {
		got := strict.fails(t, "pg_diff_schema", a)
		if !strings.HasPrefix(got, `connection "super" refused (role_check strict): its role can do more than read.`) {
			t.Errorf("%v:\n%s", a, got)
		}
	}
	for _, line := range strict.auditLines(t) {
		if line.Tool != "pg_diff_schema" || line.Outcome != "preflight_block" {
			t.Errorf("audit line %+v, want a preflight_block", line)
		}
	}
}

// Under strict, a side whose role check fails refuses the call too, and the
// error starts with that side's name.
func TestDiffSchemaNamesTheSideWhoseRoleCheckFails(t *testing.T) {
	t.Parallel()
	conns := appConns(t)
	conns["down"] = direct("postgres://u@127.0.0.1:1/none?sslmode=disable") // nothing listens on port 1
	s := connect(t, newConfig(config.RoleCheckStrict, conns))

	const want = "down: role check failed, and role_check strict refuses connections it cannot check: "
	for _, a := range []map[string]any{diffArgs("clean", "down"), diffArgs("down", "clean")} {
		if got := s.fails(t, "pg_diff_schema", a); !strings.HasPrefix(got, want) {
			t.Errorf("%v:\n%s\nwant it to start with\n%s", a, got, want)
		}
	}
	for _, line := range s.auditLines(t) {
		if line.Outcome != "db_error" {
			t.Errorf("audit line %+v, want a db_error", line)
		}
	}
}

// These fail before any query, so they need no database.
func TestDiffSchemaInvalidCalls(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"db": direct(unusedDSN), "app": direct(unusedDSN)}))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{diffArgs("nope", "db"), `unknown connection "nope" (available: app, db)`},
		{diffArgs("db", "nope"), `unknown connection "nope" (available: app, db)`},
		{map[string]any{"left": "db"}, `unknown connection ""`},
		{diffArgs("db", "db"), "left and right must name two different connections"},
		{diffArgs("db", "app", "table", "orders"), `invalid arguments: json: unknown field "table"`},
		{diffArgs("db", "app", "schema", 1), "invalid arguments"},
	} {
		if got := s.fails(t, "pg_diff_schema", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%v = %q, want it to contain %q", tc.args, got, tc.want)
		}
	}
	// No connection is recorded until both are known and differ.
	for _, line := range s.auditLines(t) {
		if line.Connection != "" || line.Outcome != "error" {
			t.Errorf("audit line %+v, want an error with no connection", line)
		}
	}
}

// Postgres cuts a name to 63 bytes, so a longer schema argument matched the
// schema its first 63 bytes name, and the first line repeated it whole. It is
// refused; a schema of 63 bytes is compared.
func TestDiffSchemaRefusesANameLongerThanPostgresKeeps(t *testing.T) {
	t.Parallel()
	schema := strings.Repeat("s", 63)
	testdb.CreateDatabase(t, "tools_diff_name_left", "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".a (id int);")
	testdb.CreateDatabase(t, "tools_diff_name_right", "CREATE SCHEMA "+schema+";")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"left":  direct(testdb.DSN(t, testdb.Superuser, "tools_diff_name_left")),
		"right": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_name_right")),
	}))

	want := "schema " + schema + ", left → right: 1 difference (- only in left, + only in right)\n- table a"
	if got := s.ok(t, "pg_diff_schema", diffArgs("left", "right", "schema", schema)); got != want {
		t.Errorf("a 63-byte schema:\n%s\nwant\n%s", got, want)
	}
	want = "schema is 64 bytes: Postgres names take at most 63, and it cuts a longer one"
	if got := s.fails(t, "pg_diff_schema", diffArgs("left", "right", "schema", schema+"x")); got != want {
		t.Errorf("a 64-byte schema = %q, want %q", got, want)
	}
}

// A diff past the inline limit is spilled to a CSV file, one difference per
// row, under the line that says which side is which.
func TestDiffSchemaSpillsALargeDiff(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_diff_many", `DO $$ BEGIN FOR i IN 1..500 LOOP
		EXECUTE format('CREATE TABLE spilled_table_%s (id int)', lpad(i::text, 3, '0'));
	END LOOP; END $$`)
	testdb.CreateDatabase(t, "tools_diff_none", "SELECT 1")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"many": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_many")),
		"none": direct(testdb.DSN(t, testdb.Superuser, "tools_diff_none")),
	}))

	got := s.ok(t, "pg_diff_schema", diffArgs("many", "none"))
	head, summary, _ := strings.Cut(got, "\n")
	if want := "schema public, many → none: 500 differences (- only in many, + only in none)"; head != want {
		t.Errorf("first line = %q, want %q", head, want)
	}
	if len(summary) > config.InlineMaxBytes || !strings.HasPrefix(summary, "Result too large to return inline (") ||
		!strings.Contains(summary, "500 rows × 1 column, truncated=false.") {
		t.Fatalf("summary (%d bytes):\n%s", len(summary), summary)
	}
	records := readSpill(t, spillPath(t, summary), s.spillDir)
	if len(records) != 501 {
		t.Fatalf("%d CSV records, want a header and 500 rows", len(records))
	}
	for i, want := range map[int]string{0: "difference", 1: "- table spilled_table_001", 500: "- table spilled_table_500"} {
		if got := strings.Join(records[i], ","); got != want {
			t.Errorf("record %d = %q, want %q", i, got, want)
		}
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Rows != 500 {
		t.Errorf("audit lines = %+v, want one with 500 rows", lines)
	}
}

func TestDiffSchemaDefinitionIsSmall(t *testing.T) {
	if _, size := listedTool(t, "pg_diff_schema"); size > 700 {
		t.Errorf("pg_diff_schema takes %d bytes of JSON, want at most 700", size)
	}
}
