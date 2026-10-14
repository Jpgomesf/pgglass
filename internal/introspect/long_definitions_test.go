package introspect_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

var longDefinitionsSQL = `DO $$ BEGIN
	EXECUTE 'CREATE TYPE many AS ENUM (' || (SELECT string_agg(quote_literal('l' || lpad(g::text, 4, '0')), ', ') FROM generate_series(1, 2501) g) || ')';
END $$;
CREATE TABLE t (
	id int,
	at_cap text DEFAULT '` + strings.Repeat("a", 9_992) + `',
	past text DEFAULT '` + strings.Repeat("b", 10_000) + `',
	g text GENERATED ALWAYS AS ('` + strings.Repeat("c", 10_000) + `' || past) STORED,
	m many,
	CONSTRAINT big CHECK (past <> '` + strings.Repeat("d", 10_000) + `')
);
CREATE INDEX big_idx ON t (id) WHERE past <> '` + strings.Repeat("e", 10_000) + `';
CREATE POLICY big_policy ON t USING (past <> '` + strings.Repeat("f", 10_000) + `') WITH CHECK (past <> '` + strings.Repeat("g", 10_000) + `');
CREATE VIEW cap_view AS SELECT '` + strings.Repeat("h", 1_978) + `'::text AS c;
CREATE VIEW past_view AS SELECT '` + strings.Repeat("i", 1_979) + `'::text AS c;`

// A description reads from the catalog only what it can show: a view's query
// cut to its first 2000 characters, with a note of how many it has, as Render
// showed it; every other expression cut to its first 10,000, longer than any
// line of a 10 KB description, with a note of how many it drops; and the
// first 2500 labels of an enum, more than such a line holds. However long its
// definitions, a relation's description keeps far within max_bytes.
func TestLongDefinitionsAreCut(t *testing.T) {
	const name = "introspect_long_definitions"
	testdb.CreateDatabase(t, name, longDefinitionsSQL)
	admin := testdb.Conn(t, testdb.Superuser, name)
	if _, err := admin.Exec(t.Context(), "SET search_path = pg_catalog"); err != nil {
		t.Fatal(err)
	}
	whole := func(sql string) string { return testdb.Value[string](t, admin, sql) }
	cut := func(s string) string {
		if len(s) <= 10_000 {
			return s
		}
		return s[:10_000] + fmt.Sprintf("...[+%d chars]", len(s)-10_000)
	}
	expr := func(column string) string {
		return whole("SELECT pg_get_expr(adbin, adrelid, true) FROM pg_attrdef WHERE adrelid = 'public.t'::regclass AND adnum = " +
			"(SELECT attnum FROM pg_attribute WHERE attrelid = 'public.t'::regclass AND attname = '" + column + "')")
	}
	q := testdb.Querier(t, testdb.Superuser, name)

	d := describe(t, q, "public", "t")
	if atCap := expr("at_cap"); len(atCap) != 10_000 || d.Columns[1].Default != atCap {
		t.Errorf("a default of %d characters = %.40q...", len(atCap), d.Columns[1].Default)
	}
	equal(t, "a default over 10,000 characters", d.Columns[2].Default, cut(expr("past")))
	equal(t, "a generation expression", d.Columns[3].Generated, "GENERATED ALWAYS AS ("+cut(expr("g"))+") STORED")
	labels := make([]string, 2500)
	for i := range labels {
		labels[i] = fmt.Sprintf("l%04d", i+1)
	}
	equal(t, "the labels of an enum of 2501", d.Columns[4].EnumValues, labels)
	equal(t, "a constraint", d.Constraints[0].Definition, cut(whole("SELECT pg_get_constraintdef(oid, true) FROM pg_constraint WHERE conname = 'big'")))
	equal(t, "an index", d.Indexes[0].Definition, cut(whole("SELECT pg_get_indexdef('public.big_idx'::regclass, 0, true)")))
	equal(t, "a policy's USING", d.Policies[0].Using, cut(whole("SELECT pg_get_expr(polqual, polrelid, true) FROM pg_policy")))
	equal(t, "a policy's WITH CHECK", d.Policies[0].WithCheck, cut(whole("SELECT pg_get_expr(polwithcheck, polrelid, true) FROM pg_policy")))

	if atCap := whole("SELECT pg_get_viewdef('public.cap_view'::regclass, true)"); len(atCap) != 2000 || describe(t, q, "public", "cap_view").Definition != atCap {
		t.Errorf("the definition of a view of %d characters is not whole", len(atCap))
	}
	past := whole("SELECT pg_get_viewdef('public.past_view'::regclass, true)")
	equal(t, "the definition of a view of 2001 characters", describe(t, q, "public", "past_view").Definition,
		past[:2000]+"\n-- truncated: 2000 of 2001 characters shown")
}
