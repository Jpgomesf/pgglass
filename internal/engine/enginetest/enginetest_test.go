package enginetest_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/engine/enginetest"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func query(t *testing.T, q engine.Querier, query engine.Query) *result.Result {
	t.Helper()
	res, err := q.Query(t.Context(), query)
	if err != nil {
		t.Fatalf("Query(%q): %v", query.SQL, err)
	}
	return res
}

// firstRow returns the first row of res, which must have as many cells as columns.
func firstRow(t *testing.T, res *result.Result) []*string {
	t.Helper()
	if len(res.Rows) == 0 || len(res.Rows[0]) != len(res.Columns) {
		t.Fatalf("want a first row with %d cells, got rows %v", len(res.Columns), res.Rows)
	}
	return res.Rows[0]
}

func TestQuerierReturnsPostgresText(t *testing.T) {
	q := enginetest.Querier(t, testdb.DSN(t, testdb.RoleClean, testdb.DBApp))

	res := query(t, q, engine.Query{SQL: `
		SELECT c.id, o.total, o.quantities, o.metadata, o.receipt, o.status
		FROM app.orders o JOIN app.customers c ON c.id = o.customer_id
		WHERE o.id = $1`, Params: []any{6}})

	want := []struct {
		name, typeName string
		value          *string
	}{
		{"id", "uuid", ptr("0ddc4d69-0dcf-fba9-15c2-88a68e6665de")}, // md5('customer-7')
		{"total", "numeric", ptr("74.04")},
		{"quantities", "_int4", ptr("{1,2}")},
		{"metadata", "jsonb", nil},
		{"receipt", "bytea", ptr(`\xd792b02dca848ba0e83462f1c04ef0a6`)}, // md5('receipt-6')
		{"status", "", ptr("shipped")},                                  // enum: unknown to pgx's type map
	}
	if len(res.Columns) != len(want) {
		t.Fatalf("columns = %+v", res.Columns)
	}
	for i, w := range want {
		col := res.Columns[i]
		if col.Name != w.name || col.TypeName != w.typeName || col.TypeOID == 0 {
			t.Errorf("column %d = %+v, want name %q type %q", i, col, w.name, w.typeName)
		}
		if got := firstRow(t, res)[i]; !equal(got, w.value) {
			t.Errorf("%s = %v, want %v", w.name, deref(got), deref(w.value))
		}
	}
}

func TestQuerierKeepsDuplicateColumns(t *testing.T) {
	q := enginetest.Querier(t, testdb.DSN(t, testdb.RoleClean, testdb.DBApp))
	res := query(t, q, engine.Query{SQL: "SELECT 1 AS a, NULL::text AS a, '' AS a"})
	if len(res.Columns) != 3 || res.Columns[0].Name != "a" || res.Columns[2].Name != "a" {
		t.Fatalf("columns = %+v", res.Columns)
	}
	if v := firstRow(t, res)[1]; v != nil {
		t.Errorf("NULL came back as %q", *v)
	}
	if v := firstRow(t, res)[2]; v == nil || *v != "" {
		t.Errorf("empty string came back as %v", v)
	}
}

func TestQuerierAppliesSearchPath(t *testing.T) {
	q := enginetest.Querier(t, testdb.DSN(t, testdb.RoleClean, testdb.DBApp))
	res := query(t, q, engine.Query{SQL: "SELECT count(*) FROM customers", SearchPath: "app"})
	if v := firstRow(t, res)[0]; v == nil || *v != "50" {
		t.Errorf("count = %v, want 50", deref(v))
	}
	if _, err := q.Query(t.Context(), engine.Query{SQL: "SELECT count(*) FROM customers"}); err == nil {
		t.Error("search_path leaked into the next query")
	}
}

func TestQuerierIsReadOnly(t *testing.T) {
	q := enginetest.Querier(t, testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp))
	_, err := q.Query(t.Context(), engine.Query{SQL: "INSERT INTO app.audit_log (message) VALUES ('nope')"})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Errorf("INSERT error = %v, want read_only_sql_transaction (25006)", err)
	}
}

func ptr(s string) *string { return &s }

func equal(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}
