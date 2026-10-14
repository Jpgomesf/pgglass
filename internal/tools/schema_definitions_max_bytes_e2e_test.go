package tools_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_schema shows the first 2000 characters of a view's definition and only
// the lines that fit in 10 KB, but read every definition whole from the
// catalog: one over max_bytes, such as a long IN list in a view or a CHECK
// constraint, or an enum of 20,000 labels, failed the description and the
// resource read of the relation.
func TestSchemaDescribesRelationsWhoseDefinitionsExceedMaxBytes(t *testing.T) {
	t.Parallel()
	const db = "tools_schema_definitions_max_bytes"
	ids := make([]string, 140_000)
	for i := range ids {
		ids[i] = strconv.Itoa(1_000_000 + i)
	}
	in := "IN (" + strings.Join(ids, ", ") + ")"
	testdb.CreateDatabase(t, db, "CREATE TABLE small (id int);\n"+
		"CREATE VIEW big AS SELECT id FROM small WHERE id "+in+";\n"+
		"CREATE TABLE checked (id int CONSTRAINT listed CHECK (id "+in+"), note text);\n"+
		`DO $$ BEGIN
			EXECUTE 'CREATE TYPE many AS ENUM (' || (SELECT string_agg(quote_literal('label_' || lpad(g::text, 50, '0')), ', ') FROM generate_series(1, 20000) g) || ')';
		END $$;
		CREATE TABLE labelled (id int, m many, note text);`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, db))}))

	for _, tc := range []struct {
		table string
		want  *regexp.Regexp
	}{
		{"big", regexp.MustCompile(`(?s)^public\.big: view\n  id integer\ndefinition:\n SELECT .+\n-- truncated: 2000 of \d{7} characters shown$`)},
		{"checked", regexp.MustCompile(`^public\.checked: table, [^\n]+\n  id integer\n  note text\nconstraints:\n… 4 of 5 lines shown \(output limit 10000 bytes\): use pg_search to find names$`)},
		{"labelled", regexp.MustCompile(`^public\.labelled: table, [^\n]+\n  id integer\n… 2 of 4 lines shown \(output limit 10000 bytes\): use pg_search to find names$`)},
	} {
		got, isErr := s.call(t, "pg_schema", map[string]any{"connection": "db", "schema": "public", "table": tc.table})
		if isErr || !tc.want.MatchString(got) || len(got) > config.InlineMaxBytes {
			t.Errorf("pg_schema of %s: isError %t, %d bytes:\n%.300s", tc.table, isErr, len(got), got)
			continue
		}
		res, err := s.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "pgglass://db/public/" + tc.table})
		if err != nil {
			t.Errorf("resources/read of %s: %v", tc.table, err)
		} else if text := res.Contents[0].Text; text != got {
			t.Errorf("resources/read of %s:\n%.300s\nwant what pg_schema returns", tc.table, text)
		}
	}
}
