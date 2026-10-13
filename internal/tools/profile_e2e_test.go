package tools_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// unanalyzedFixture holds tables that pg_stats has nothing on: one smaller
// and one larger than a sample.
const unanalyzedFixture = `
CREATE TABLE fresh (id int, note text) WITH (autovacuum_enabled = false);
INSERT INTO fresh SELECT g, CASE WHEN g % 3 <> 0 THEN 'n' || g % 7 END FROM generate_series(1, 300) AS g;
CREATE TABLE big (id int) WITH (autovacuum_enabled = false);
INSERT INTO big SELECT g FROM generate_series(1, 12000) AS g;
GRANT SELECT ON fresh, big TO ro_clean;
`

// wideFixture holds a table with more columns than pg_profile shows, whose
// common values are long enough to make its profile too large to return
// inline: c01 to c60, each with five values of 300 characters.
const wideFixture = `
DO $$ BEGIN
	EXECUTE 'CREATE TABLE wide (' || (SELECT string_agg(format('c%s text', lpad(i::text, 2, '0')), ', ')
		FROM generate_series(1, 60) AS i) || ')';
	EXECUTE 'INSERT INTO wide SELECT ' || (SELECT string_agg('repeat(chr(65 + g % 5), 300)', ', ')
		FROM generate_series(1, 60)) || ' FROM generate_series(1, 100) AS g';
END $$;
ANALYZE wide;
GRANT SELECT ON wide TO ro_clean;
`

// profileConns creates database name from fixture and connects to it as
// ro_clean ("profile"), and to the app fixture as ro_clean (with
// default_schema app), as ro_supabase, which may read none of it, and as the
// superuser.
func profileConns(t *testing.T, name, fixture string) map[string]config.Connection {
	t.Helper()
	testdb.CreateDatabase(t, name, fixture)
	conns := appConns(t)
	conns["supa"] = direct(testdb.DSN(t, testdb.RoleSupabase, testdb.DBApp))
	conns["profile"] = direct(testdb.DSN(t, testdb.RoleClean, name))
	return conns
}

// ordersProfile is the profile of app.orders. The fixture was analyzed with
// every row in ANALYZE's sample, so it is exact. A column without common
// values or a histogram has empty cells for them, and TSV writes the
// backslash that starts a bytea value as \\.
const ordersProfile = "app.orders: ~50 rows, from pg_stats\n" +
	"column\ttype\tnulls\tdistinct\twidth\tcommon\tmin\tmax\n" +
	"id\tbigint\t0%\t100%\t8\t\t1\t50\n" +
	"customer_id\tuuid\t0%\t100%\t16\t\t0abe215d-4832-1215-fe7a-264bfb844be9\tf834df2d-140a-bfc5-8ab7-07c5a2941b4d\n" +
	"status\tapp.order_status\t0%\t4\t4\t'paid' 26%, 'shipped' 26%, 'pending' 24%, 'cancelled' 24%\t\t\n" +
	"total\tnumeric(12,2)\t0%\t100%\t6\t\t12.34\t617.00\n" +
	"quantities\tinteger[]\t0%\t30%\t29\t'{1,4}' 8%, '{2,2}' 8%, '{2,5}' 8%, '{3,1}' 8%, '{3,3}' 8%\t\t\n" +
	"metadata\tjsonb\t16%\t4\t45\t" + `'{"items": 2, "channel": "store"}' 26%, '{"items": 4, "channel": "store"}' 24%, ` +
	`'{"items": 3, "channel": "web"}' 18%, '{"items": 1, "channel": "web"}' 16%` + "\t\t\n" +
	"fulfillment\tinterval\t74%\t26%\t16\t\t18:00:00\t8 days 02:00:00\n" +
	"receipt\tbytea\t68%\t32%\t17\t\t\\\\x46a6ee8e62b61a756fb7c03d8d26e8b8\t\\\\xf3976a2e441d40a4135faeac5e182028\n" +
	"placed_at\ttimestamp with time zone\t0%\t100%\t8\t\t2024-03-01 15:00:00+00\t2024-03-13 21:00:00+00\n" +
	"(9 rows)"

const sampledFrom = " with TABLESAMPLE SYSTEM (100) LIMIT 10000, since pg_stats has nothing on these columns " +
	"(never analyzed, or hidden by row-level security)\n"

func TestProfile(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, profileConns(t, "tools_profile", unanalyzedFixture)))
	for _, tc := range []struct {
		args    map[string]any
		want    string // the whole result, or for an error a part of it
		outcome string
		rows    int
	}{
		{map[string]any{"connection": "clean", "table": "orders"}, ordersProfile, "ok", 9},
		{map[string]any{"connection": "clean", "table": "app.customers", "column": "active"}, "app.customers: ~50 rows, from pg_stats\n" +
			"column\ttype\tnulls\tdistinct\twidth\tcommon\tmin\tmax\nactive\tboolean\t0%\t2\t1\t't' 80%, 'f' 20%\t\t\n(1 row)", "ok", 1},
		{map[string]any{"connection": "clean", "table": "customers", "schema": "app", "column": "profile"}, "app.customers: ~50 rows, from pg_stats\n" +
			"column\ttype\tnulls\tdistinct\twidth\tcommon\tmin\tmax\nprofile\tjson\t10%\t\t36\t\t\t\n(1 row)", "ok", 1},
		// Never analyzed: every row is read, up to the LIMIT.
		{map[string]any{"connection": "profile", "table": "public.fresh"}, "public.fresh: sampled 300 rows" + sampledFrom +
			"column\ttype\tnulls\tdistinct\nid\tinteger\t0\t300\nnote\ttext\t100\t7\n(2 rows)", "ok", 2},
		{map[string]any{"connection": "profile", "table": "public.big"}, "public.big: sampled 10000 rows" + sampledFrom +
			"column\ttype\tnulls\tdistinct\nid\tinteger\t0\t10000\n(1 row)", "ok", 1},
		// Row-level security hides the statistics from ro_clean, and the sample
		// holds only the rows it may see: tenant acme's.
		{map[string]any{"connection": "clean", "table": "tenant_notes", "column": "tenant"}, "app.tenant_notes: sampled 25 rows" + sampledFrom +
			"column\ttype\tnulls\tdistinct\ntenant\ttext\t0\t1\n(1 row)", "ok", 1},
		{map[string]any{"connection": "clean", "table": "nope"}, `relation "app"."nope" does not exist or is not a table or materialized view`, "db_error", 0},
		{map[string]any{"connection": "clean", "table": "customer_order_totals"}, `relation "app"."customer_order_totals" does not exist or is not a table`, "db_error", 0},
		{map[string]any{"connection": "clean", "table": "orders", "column": "nope"}, `column "nope" of "app"."orders" does not exist or the current role cannot SELECT it: ` +
			"pg_stats shows only the columns a role can SELECT", "db_error", 0},
		{map[string]any{"connection": "supa", "table": "app.orders"}, `the current role can SELECT no column of "app"."orders": ` +
			"pg_stats shows only the columns a role can SELECT", "db_error", 0},
		// The superuser may read pg_authid, but its statistics hold password
		// verifiers, as its rows do.
		{map[string]any{"connection": "super", "table": "pg_catalog.pg_authid"}, "query rejected: relation pg_authid is not allowed: " +
			"it discloses server configuration or credentials", "guard_reject", 0},
		{map[string]any{"connection": "profile", "table": "fresh"}, "table needs a schema: pass schema.table, or the schema that holds it", "error", 0},
		{map[string]any{"connection": "clean"}, "table must not be empty", "error", 0},
		{map[string]any{"connection": "clean", "table": "orders", "columns": "id"}, `invalid arguments: json: unknown field "columns"`, "error", 0},
	} {
		text, isErr := s.call(t, "pg_profile", tc.args)
		switch {
		case tc.outcome == "ok" && (isErr || text != tc.want):
			t.Errorf("pg_profile %v:\n%s\nwant\n%s", tc.args, text, tc.want)
		case tc.outcome != "ok" && (!isErr || !strings.Contains(text, tc.want)):
			t.Errorf("pg_profile %v = %q (error: %v), want an error containing %q", tc.args, text, isErr, tc.want)
		}
		if strings.Contains(text, "SCRAM-SHA-256") {
			t.Errorf("pg_profile %v shows a password verifier:\n%s", tc.args, text)
		}
		lines := s.auditLines(t)
		line := lines[len(lines)-1]
		if line.Tool != "pg_profile" || line.Outcome != tc.outcome || line.Rows != tc.rows || line.SQLLen != 0 {
			t.Errorf("pg_profile %v: audit line %+v, want outcome %s and %d rows", tc.args, line, tc.outcome, tc.rows)
		}
	}
}

// The first 50 columns, inline or, when that is too long, in a spill file.
func TestProfileSpillsAndCountsTheColumnsLeftOut(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, profileConns(t, "tools_profile_wide", wideFixture)))
	text := s.ok(t, "pg_profile", map[string]any{"connection": "profile", "table": "public.wide"})
	head := "public.wide: ~100 rows, from pg_stats; 10 more columns not shown: pass column to profile one\n" +
		"Result too large to return inline ("
	if !strings.HasPrefix(text, head) || !strings.Contains(text, "50 rows × 8 columns, truncated=false.") || len(text) > config.InlineMaxBytes {
		t.Fatalf("pg_profile of 60 wide columns (%d bytes):\n%s", len(text), text)
	}
	records := readSpill(t, spillPath(t, text), s.spillDir)
	if len(records) != 51 || strings.Join(records[0], ",") != "column,type,nulls,distinct,width,common,min,max" {
		t.Fatalf("spill file: %d records, header %q", len(records), records[0])
	}
	// The file keeps the common values whole: five of 300 characters each.
	c01 := records[1]
	if c01[0] != "c01" || c01[2] != "0%" || c01[3] != "5" || len(c01[5]) != 5*len("'"+strings.Repeat("A", 300)+"' 20%")+4*len(", ") {
		t.Errorf("first record = %q", c01)
	}
	if last := records[50]; last[0] != "c50" {
		t.Errorf("last record is %s, want c50", last[0])
	}
}

func TestProfileRoleCheckStrictRefusesRolesWithCriticalFindings(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	got := s.fails(t, "pg_profile", map[string]any{"connection": "super", "table": "app.orders"})
	if !strings.HasPrefix(got, `connection "super" refused (role_check strict): its role can do more than read.`) {
		t.Errorf("pg_profile as the superuser under role_check strict:\n%s", got)
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Outcome != "preflight_block" {
		t.Errorf("audit lines = %+v, want one preflight_block", lines)
	}
}

func TestProfileDefinitionIsSmall(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	res, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "pg_profile" {
			continue
		}
		def, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if len(def) >= 700 {
			t.Errorf("pg_profile definition takes %d bytes of JSON, want under 700", len(def))
		}
		t.Logf("pg_profile definition: %d bytes of JSON", len(def))
		return
	}
	t.Fatal("pg_profile is not listed")
}
