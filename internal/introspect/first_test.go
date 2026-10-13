package introspect_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/introspect"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestFirstListingsRejectALimitBelowOne(t *testing.T) {
	q := &fakeQuerier{err: errors.New("must not be queried")}
	for _, limit := range []int{0, -1} {
		_, _, err := introspect.FirstSchemas(context.Background(), q, limit)
		wantErr(t, err, "limit must be at least 1")
		_, _, err = introspect.FirstTables(context.Background(), q, "app", limit)
		wantErr(t, err, "limit must be at least 1")
	}
}

// The first schemas or relations come back by name, with how many there
// are in all; a limit past them all returns them all.
func TestFirstSchemasAndTables(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	schemas, total, err := introspect.FirstSchemas(t.Context(), q, 1)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "FirstSchemas(1)", schemas, introspect.SchemaList{{Name: "app", Relations: 7, Comment: "Fixture application schema"}})
	equal(t, "schemas in all", total, 2)

	relations, total, err := introspect.FirstTables(t.Context(), q, "app", 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range relations {
		relations[i].Size = "" // varies by version
	}
	equal(t, "FirstTables(app, 2)", relations, introspect.RelationList{
		{Schema: "app", Name: "audit_log", Kind: "table", Rows: 50},
		{Schema: "app", Name: "customer_order_totals", Kind: "view", Rows: -1},
	})
	equal(t, "relations in all", total, 7)

	relations, total, err = introspect.FirstTables(t.Context(), q, "app", 100)
	if err != nil || len(relations) != 7 || total != 7 {
		t.Errorf("FirstTables(app, 100) = %d relations of %d, %v; want all 7", len(relations), total, err)
	}

	_, _, err = introspect.FirstTables(t.Context(), q, "nope", 2)
	wantErr(t, err, `schema "nope" does not exist or the current role lacks USAGE on it`)
}
