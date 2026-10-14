package tools_test

import (
	"encoding/json"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// resources/list names the first relations of each connection in use, and an
// agent can read that list, as with Claude Code's ListMcpResourcesTool: 200
// ordinary table names would take 30 KB of it, so it keeps to the inline limit
// too.
func TestResourceListStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const db = "tools_resource_list_inline_bound"
	testdb.CreateDatabase(t, db, `CREATE SCHEMA app;
	DO $$ BEGIN FOR i IN 1..200 LOOP
		EXECUTE format('CREATE TABLE app.%I (id int)', 'customer_orders_' || lpad(i::text, 4, '0'));
	END LOOP; END $$;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"prod-app": direct(testdb.DSN(t, testdb.Superuser, db))}))
	s.ok(t, "pg_query", args("prod-app", "SELECT 1"))

	res, err := s.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resources) == 0 || len(wire) > config.InlineMaxBytes {
		t.Errorf("resources/list after a call on a connection of 200 relations: %d resources, %d bytes of JSON, limit %d",
			len(res.Resources), len(wire), config.InlineMaxBytes)
	}
}
