package introspect_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Listings and descriptions cut comments past 200 characters, not bytes, with a
// note of what was dropped, keeping catalog results within max_bytes.
func TestLongCommentsAreCut(t *testing.T) {
	const name = "introspect_long_comments"
	testdb.CreateDatabase(t, name, `CREATE SCHEMA s;
		CREATE TABLE s.t (a int, b int);
		COMMENT ON SCHEMA s IS '`+strings.Repeat("s", 201)+`';
		COMMENT ON TABLE s.t IS '`+strings.Repeat("é", 250)+`';
		COMMENT ON COLUMN s.t.a IS '`+strings.Repeat("a", 200)+`';
		COMMENT ON COLUMN s.t.b IS '`+strings.Repeat("b", 1000)+`';`)
	q := testdb.Querier(t, testdb.Superuser, name)
	schemaComment := strings.Repeat("s", 200) + "...[+1 chars]"
	tableComment := strings.Repeat("é", 200) + "...[+50 chars]"

	schemas, _, err := introspect.FirstSchemas(t.Context(), q, everything)
	if err != nil || len(schemas) != 2 || schemas[1].Name != "s" {
		t.Fatalf("FirstSchemas = %+v, %v; want public and s", schemas, err)
	}
	equal(t, "schema comment", schemas[1].Comment, schemaComment)

	relations, _, err := introspect.FirstTables(t.Context(), q, "s", everything)
	if err != nil || len(relations) != 1 {
		t.Fatalf("FirstTables(s) = %+v, %v; want s.t", relations, err)
	}
	equal(t, "table comment in the listing", relations[0].Comment, tableComment)

	d := describe(t, q, "s", "t")
	equal(t, "table comment in the description", d.Comment, tableComment)
	equal(t, "column comments", []string{d.Columns[0].Comment, d.Columns[1].Comment},
		[]string{strings.Repeat("a", 200), strings.Repeat("b", 200) + "...[+800 chars]"})
}
