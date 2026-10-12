package guard

import (
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// The guard relies on github.com/wasilibs/go-pgquery: libpg_query compiled to
// WebAssembly, so the server keeps building with CGO_ENABLED=0. These tests
// pin what the guard needs from it: PG 17 syntax, statement counts and errors.

func parseOne(t *testing.T, sql string) *pg_query.Node {
	t.Helper()
	tree, err := pgquery.Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		t.Fatalf("Parse(%q): %d statements, want 1", sql, n)
	}
	return tree.GetStmts()[0].GetStmt()
}

func TestParserHandlesPostgres17Syntax(t *testing.T) {
	jsonTable := parseOne(t, `SELECT jt.* FROM docs,
		JSON_TABLE(docs.body, '$.items[*]' COLUMNS (id int PATH '$.id', name text PATH '$.name')) AS jt`)
	if jsonTable.GetSelectStmt() == nil {
		t.Errorf("JSON_TABLE query parsed as %T, want SelectStmt", jsonTable.GetNode())
	}

	merge := parseOne(t, `MERGE INTO t USING s ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET v = s.v
		WHEN NOT MATCHED BY SOURCE THEN DELETE
		RETURNING merge_action(), t.*`)
	if merge.GetMergeStmt() == nil {
		t.Errorf("MERGE ... RETURNING parsed as %T, want MergeStmt", merge.GetNode())
	}
}

func TestParserExposesWritesHiddenInSelect(t *testing.T) {
	stmt := parseOne(t, "WITH gone AS (DELETE FROM t RETURNING *) SELECT * FROM gone").GetSelectStmt()
	if stmt == nil {
		t.Fatal("want a SelectStmt")
	}
	ctes := stmt.GetWithClause().GetCtes()
	if len(ctes) != 1 || ctes[0].GetCommonTableExpr().GetCtequery().GetDeleteStmt() == nil {
		t.Errorf("data-modifying CTE not visible in the tree: %v", ctes)
	}
}

func TestParserCountsStatementsAndReportsErrors(t *testing.T) {
	tree, err := pgquery.Parse("SELECT 1; SET statement_timeout = 0")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tree.GetStmts()); n != 2 {
		t.Errorf("%d statements, want 2", n)
	}
	if tree.GetStmts()[1].GetStmt().GetVariableSetStmt() == nil {
		t.Error("SET not parsed as VariableSetStmt")
	}

	if _, err := pgquery.Parse("SELEC 1"); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("Parse of invalid SQL: err = %v, want a syntax error", err)
	}
}
