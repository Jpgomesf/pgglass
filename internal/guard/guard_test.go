package guard_test

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/guard/guardtest"
)

func assertRejected(t *testing.T, sql, want string) {
	t.Helper()
	err := guard.Check(sql)
	var v *guard.Violation
	if !errors.As(err, &v) {
		t.Errorf("Check(%q) = %v, want a *Violation", sql, err)
		return
	}
	if !strings.Contains(v.Reason, want) {
		t.Errorf("Check(%q) reason = %q, want it to contain %q", sql, v.Reason, want)
	}
}

func TestCheckRejectsBypassCorpus(t *testing.T) {
	for _, c := range guardtest.Rejected {
		assertRejected(t, c.SQL, c.Want)
	}
}

func TestCheckAllowsReadOnlyCorpus(t *testing.T) {
	for _, sql := range guardtest.Allowed {
		if err := guard.Check(sql); err != nil {
			t.Errorf("Check(%q) = %v, want nil", sql, err)
		}
	}
}

func TestCheckRejectsInputItCannotParse(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"SELEC 1", `does not parse: syntax error at or near "SELEC" at character 1`},
		{"SELECT (", "does not parse"},
		// The lexer fails first, before the parser runs.
		{"SELECT 'unterminated", "does not parse: unterminated quoted string"},
		{"", "exactly one statement, got 0"},
		{" \n\t", "got 0"},
		{"-- SELECT 1", "got 0"},
		{";;", "got 0"},
		// The parser reads a C string, so it would only see "SELECT 1".
		{"SELECT 1\x00; DROP TABLE app.orders", "NUL byte"},
		{"SELECT '\xff'", "not valid UTF-8"},
		{"SELECT 1 -- \xc3", "not valid UTF-8"},
	} {
		assertRejected(t, c.sql, c.want)
	}
}

func TestCheckRejectsSQLTooDeepToParse(t *testing.T) {
	// At these depths the parser's stack overflows, and the overflowed
	// instance can hang the next parse, so they must not reach the parser.
	for _, sql := range []string{
		"SELECT 1" + strings.Repeat(" + 1", 8_000),
		"SELECT " + strings.Repeat("NOT ", 8_000) + "true",
		"SELECT " + strings.Repeat("(SELECT ", 3_000) + "1" + strings.Repeat(")", 3_000),
		"SELECT " + strings.Repeat("a[", 4_000) + "1" + strings.Repeat("]", 4_000),
		"SELECT " + strings.Repeat("(", 20_000) + "1" + strings.Repeat(")", 20_000),
	} {
		assertRejected(t, sql, "nested too deeply")
	}

	// The parser still works, error paths included.
	if err := guard.Check("SELECT 1"); err != nil {
		t.Errorf("Check(SELECT 1) = %v, want nil", err)
	}
	assertRejected(t, "SELEC 1", "does not parse")
}

func TestCheckAllowsLargeFlatSQL(t *testing.T) {
	values := make([]string, 10_000)
	for i := range values {
		values[i] = "-" + strconv.Itoa(i)
	}
	rows := strings.Repeat("(1, 'name', true, NULL, now() - interval '1 day'), ", 3_000)
	for _, sql := range []string{
		"SELECT * FROM app.orders WHERE id IN (" + strings.Join(values, ", ") + ")",
		"VALUES " + rows + "(2, 'name', false, NULL, now())",
		"SELECT * FROM app.orders WHERE " + strings.Repeat("id = 1 OR ", 600) + "false",
	} {
		if err := guard.Check(sql); err != nil {
			t.Errorf("Check(%.60q...) = %v, want nil", sql, err)
		}
	}
}

func TestCheckIsSafeForConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range guardtest.Rejected[:20] {
				if guard.Check(c.SQL) == nil {
					t.Errorf("Check(%q) = nil, want a *Violation", c.SQL)
				}
			}
			for _, sql := range guardtest.Allowed[:20] {
				if err := guard.Check(sql); err != nil {
					t.Errorf("Check(%q) = %v, want nil", sql, err)
				}
			}
		}()
	}
	wg.Wait()
}

// FuzzCheck: Check never panics, every error it returns is a *Violation, and
// whatever it accepts parses to exactly one allowed statement.
func FuzzCheck(f *testing.F) {
	for _, c := range guardtest.Rejected {
		f.Add(c.SQL)
	}
	for _, sql := range guardtest.Allowed {
		f.Add(sql)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		err := guard.Check(sql)
		if err != nil {
			var v *guard.Violation
			if !errors.As(err, &v) {
				t.Fatalf("Check(%q) = %v (%T), want a *Violation", sql, err, err)
			}
			return
		}
		assertOneAllowedStatement(t, sql)
	})
}

func assertOneAllowedStatement(t *testing.T, sql string) {
	t.Helper()
	if strings.ContainsRune(sql, 0) {
		t.Fatalf("Check accepted %q, which the parser cannot see all of", sql)
	}
	tree, err := pgquery.Parse(sql)
	if err != nil {
		t.Fatalf("Check accepted %q, which does not parse: %v", sql, err)
	}
	stmts := tree.GetStmts()
	if len(stmts) != 1 {
		t.Fatalf("Check accepted %q, which has %d statements", sql, len(stmts))
	}
	switch n := stmts[0].GetStmt().GetNode().(type) {
	case *pg_query.Node_SelectStmt, *pg_query.Node_VariableShowStmt:
	case *pg_query.Node_ExplainStmt:
		if n.ExplainStmt.GetQuery().GetSelectStmt() == nil {
			t.Fatalf("Check accepted %q, which explains a %T", sql, n.ExplainStmt.GetQuery().GetNode())
		}
	default:
		t.Fatalf("Check accepted %q, which is a %T", sql, n)
	}
}
