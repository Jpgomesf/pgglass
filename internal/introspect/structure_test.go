package introspect_test

import (
	"context"
	_ "embed" // changes to the app fixture
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

//go:embed testdata/app_changes.sql
var appChangesSQL string

// structure reads the structure of schema through q, failing the test on an
// error.
func structure(t *testing.T, q engine.Querier, schema string) introspect.SchemaStructure {
	t.Helper()
	s, err := introspect.Structure(t.Context(), q, schema)
	if err != nil {
		t.Fatalf("Structure(%s): %v", schema, err)
	}
	return s
}

func TestStructureQueryPinsSearchPathAndDecodes(t *testing.T) {
	q := &fakeQuerier{res: oneValue(`{"t": {"Kind": "table", "RLS": true, "Definition": null, "Columns": {"id": "integer NOT NULL"},
		"Constraints": null, "Indexes": null, "Policies": {"p": "FOR ALL TO public USING (true)"}},
		"v": {"Kind": "view", "RLS": false, "Definition": " SELECT 1 AS n;", "Columns": {"n": "integer"},
		"Constraints": null, "Indexes": null, "Policies": null}}`)}
	got, err := introspect.Structure(context.Background(), q, "app")
	if err != nil {
		t.Fatal(err)
	}
	if q.got.SearchPath != "pg_catalog" || !reflect.DeepEqual(q.got.Params, []any{"app"}) {
		t.Errorf("query search_path = %q, params = %#v; want pg_catalog and the schema", q.got.SearchPath, q.got.Params)
	}
	equal(t, "Structure", got, introspect.SchemaStructure{
		"t": {Kind: "table", RLS: true, Columns: defs{"id": "integer NOT NULL"}, Policies: defs{"p": "FOR ALL TO public USING (true)"}},
		"v": {Kind: "view", Definition: " SELECT 1 AS n;", Columns: defs{"n": "integer"}},
	})

	failure := errors.New("connection refused")
	for _, tc := range []struct {
		q    *fakeQuerier
		want string
	}{
		{&fakeQuerier{res: &result.Result{}}, `schema "app" does not exist or the current role lacks USAGE on it`},
		{&fakeQuerier{res: &result.Result{Truncated: true, TruncatedReason: "max_bytes=1048576"}},
			`read the structure of schema "app": catalog result exceeds the configured limits (max_bytes=1048576); ` +
				"raise defaults.max_bytes in config.json to read larger schemas"},
		{&fakeQuerier{err: failure}, "connection refused"},
		{&fakeQuerier{res: oneValue(`{"t": {"Kind": "table", "Triggers": {}}}`)}, `unknown field "Triggers"`},
	} {
		got, err := introspect.Structure(context.Background(), tc.q, "app")
		wantErr(t, err, tc.want)
		if got != nil {
			t.Errorf("Structure = %v, want nil on error %v", got, err)
		}
	}
	if _, err := introspect.Structure(context.Background(), &fakeQuerier{err: failure}, "app"); !errors.Is(err, failure) {
		t.Errorf("error = %v, want it to wrap %v", err, failure)
	}
}

func TestStructureOfApp(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	got := structure(t, q, "app")
	// The view and the materialized view have their query as Table reads it,
	// which TestTableDescribesViews checks; how the server version spells the
	// materialized view's varies.
	query := func(name string) string { return describe(t, q, "app", name).Definition }
	equal(t, "Structure(app)", got, introspect.SchemaStructure{
		"audit_log": {
			Kind: "table",
			Columns: defs{
				"id":        "bigint NOT NULL GENERATED ALWAYS AS IDENTITY",
				"message":   "text NOT NULL",
				"logged_at": "timestamp with time zone NOT NULL DEFAULT now()",
			},
			Constraints: defs{"audit_log_pkey": "PRIMARY KEY (id)"},
		},
		"customer_order_totals": {
			Kind:       "view",
			Definition: query("customer_order_totals"),
			Columns:    defs{"customer_id": "uuid", "email": "text", "order_count": "bigint", "total_spent": "numeric"},
		},
		"customers": {
			Kind: "table",
			Columns: defs{
				"id":          "uuid NOT NULL",
				"email":       "text NOT NULL",
				"name":        "text NOT NULL",
				"active":      "boolean NOT NULL DEFAULT true",
				"signup_date": "date NOT NULL",
				"created_at":  "timestamp with time zone NOT NULL",
				"tags":        "text[] NOT NULL DEFAULT '{}'::text[]",
				"last_ip":     "inet",
				"profile":     "json",
			},
			Constraints: defs{"customers_pkey": "PRIMARY KEY (id)"},
			// The primary key's index is left out, but not
			// customers_email_key: a unique index with no constraint.
			Indexes: defs{
				"customers_email_key":      "CREATE UNIQUE INDEX customers_email_key ON app.customers USING btree (email)",
				"customers_lower_name_idx": "CREATE INDEX customers_lower_name_idx ON app.customers USING btree (lower(name))",
			},
		},
		"daily_event_counts": {
			Kind:       "materialized view",
			Definition: query("daily_event_counts"),
			Columns:    defs{"day": "timestamp with time zone", "kind": "text", "events": "bigint"},
		},
		"events": {
			Kind: "table",
			Columns: defs{
				"id":          "bigint NOT NULL",
				"kind":        "text NOT NULL",
				"payload":     "jsonb NOT NULL",
				"occurred_at": "timestamp with time zone NOT NULL",
			},
			Constraints: defs{"events_pkey": "PRIMARY KEY (id)"},
		},
		"orders": {
			Kind: "table",
			Columns: defs{
				"id":          "bigint NOT NULL GENERATED ALWAYS AS IDENTITY",
				"customer_id": "uuid NOT NULL",
				"status":      "app.order_status NOT NULL DEFAULT 'pending'::app.order_status",
				"total":       "numeric(12,2) NOT NULL",
				"quantities":  "integer[] NOT NULL",
				"metadata":    "jsonb",
				"fulfillment": "interval",
				"receipt":     "bytea",
				"placed_at":   "timestamp with time zone NOT NULL",
			},
			Constraints: defs{
				"orders_pkey":             "PRIMARY KEY (id)",
				"orders_customer_id_fkey": "FOREIGN KEY (customer_id) REFERENCES app.customers(id)",
				"orders_total_check":      "CHECK (total >= 0::numeric)",
			},
			Indexes: defs{
				"orders_customer_id_idx": "CREATE INDEX orders_customer_id_idx ON app.orders USING btree (customer_id)",
				"orders_pending_placed_at_idx": "CREATE INDEX orders_pending_placed_at_idx ON app.orders USING btree (placed_at)" +
					" WHERE status = 'pending'::app.order_status",
			},
		},
		"tenant_notes": {
			Kind:        "table",
			RLS:         true,
			Columns:     defs{"id": "integer NOT NULL", "tenant": "text NOT NULL", "note": "text NOT NULL"},
			Constraints: defs{"tenant_notes_pkey": "PRIMARY KEY (id)"},
			Policies:    defs{"tenant_notes_acme": "FOR SELECT TO public USING (tenant = 'acme'::text)"},
		},
	})
}

func TestStructureOfLab(t *testing.T) {
	loadLab(t)
	got := structure(t, testdb.Querier(t, testdb.RoleClean, labDB), "lab")
	// As in FirstTables: no partitions, and not lab.hidden, which ro_clean cannot read.
	equal(t, "relations", slices.Sorted(maps.Keys(got)),
		[]string{"Mixed Case", "child", "measurements", "parent", "remote_items", "secrets", "wide"})
	equal(t, "child", got["child"], introspect.RelationStructure{
		Kind: "table",
		RLS:  true,
		Columns: defs{
			"id":        "bigint NOT NULL GENERATED BY DEFAULT AS IDENTITY",
			"parent_id": "integer NOT NULL",
			"price":     "numeric(10,2) NOT NULL",
			"qty":       "integer NOT NULL DEFAULT 1",
			"total":     "numeric GENERATED ALWAYS AS (price * qty::numeric) STORED",
			"mood":      "lab.mood",
			"moods":     "lab.mood[] NOT NULL DEFAULT '{}'::lab.mood[]",
		},
		Constraints: defs{
			"child_pkey":            "PRIMARY KEY (id)",
			"child_parent_id_fkey":  "FOREIGN KEY (parent_id) REFERENCES lab.parent(id) ON DELETE CASCADE",
			"child_parent_mood_key": "UNIQUE (parent_id, mood)",
			"child_qty_positive":    "CHECK (qty > 0)",
		},
		// Each index implements a constraint, so none is listed.
		Policies: defs{
			"child_guard":  "AS RESTRICTIVE FOR ALL TO public USING (price >= 0::numeric) WITH CHECK (price < 1000::numeric)",
			"child_insert": "FOR INSERT TO public WITH CHECK (qty < 100)",
			"child_read":   "FOR SELECT TO ro_clean, ro_leaky USING (qty > 0)",
		},
	})
	// The exclusion constraint's index is not listed either.
	equal(t, "parent", got["parent"], introspect.RelationStructure{
		Kind:    "table",
		Columns: defs{"id": "integer NOT NULL", "name": "text NOT NULL", "active": "tstzrange"},
		Constraints: defs{
			"parent_pkey":        "PRIMARY KEY (id)",
			"parent_active_excl": "EXCLUDE USING gist (active WITH &&)",
			"parent_name_key":    "UNIQUE (name)",
		},
	})
	for name, want := range map[string]introspect.RelationStructure{
		"measurements": {
			Kind:    "partitioned table",
			Columns: defs{"id": "integer NOT NULL", "taken_on": "date NOT NULL", "value": "double precision"},
		},
		"remote_items": {Kind: "foreign table", Columns: defs{"id": "integer", "label": "text"}},
		"Mixed Case":   {Kind: "table", Columns: defs{"Weird Col": "integer"}},
	} {
		equal(t, name, got[name], want)
	}
	// ro_clean may not read secrets.token.
	equal(t, "secrets columns", got["secrets"].Columns, defs{"id": "integer NOT NULL", "label": "text"})

	// The superuser reads lab.hidden and secrets.token; partitions are still left out.
	got = structure(t, testdb.Querier(t, testdb.Superuser, labDB), "lab")
	equal(t, "relations as the superuser", slices.Sorted(maps.Keys(got)),
		[]string{"Mixed Case", "child", "hidden", "measurements", "parent", "remote_items", "secrets", "wide"})
	if def := got["secrets"].Columns["token"]; def != "text" {
		t.Errorf("secrets.token as the superuser = %q, want text", def)
	}
}

func TestStructureRejectsUnusableSchemas(t *testing.T) {
	loadLab(t)
	q := testdb.Querier(t, testdb.RoleClean, labDB)
	for _, schema := range []string{"locked", "nope"} {
		got, err := introspect.Structure(t.Context(), q, schema)
		wantErr(t, err, fmt.Sprintf("schema %q does not exist or the current role lacks USAGE on it", schema))
		if got != nil {
			t.Errorf("Structure(%s) = %v, want nil", schema, got)
		}
	}
}

// A copy of the app fixture with changes of every kind: Diff finds each one
// and nothing else, both ways.
func TestDiffFindsChangesToACopyOfTheAppFixture(t *testing.T) {
	const name = "introspect_app_changed"
	testdb.CreateDatabase(t, name, testdb.AppSQL()+appChangesSQL)
	app := structure(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app")
	changed := structure(t, testdb.Querier(t, testdb.RoleClean, name), "app")

	// The view's query on one line, before and after the change.
	const (
		leftJoin = "SELECT c.id AS customer_id, c.email, count(o.id) AS order_count, " +
			"COALESCE(sum(o.total), 0::numeric) AS total_spent FROM app.customers c " +
			"LEFT JOIN app.orders o ON o.customer_id = c.id GROUP BY c.id, c.email;"
		innerJoin = "SELECT c.id AS customer_id, c.email, count(o.id) AS order_count, " +
			"COALESCE(sum(o.total), 0::numeric) AS total_spent FROM app.customers c " +
			"JOIN app.orders o ON o.customer_id = c.id GROUP BY c.id, c.email;"
	)
	want := []string{
		"- table audit_log",
		"+ table coupons",
		"~ view customer_order_totals: " + leftJoin + " → " + innerJoin,
		"~ column customers.active: boolean NOT NULL DEFAULT true → boolean NOT NULL DEFAULT false",
		"~ column customers.name: text NOT NULL → text",
		"- index customers.customers_lower_name_idx",
		"+ index events.events_kind_idx",
		"+ column orders.note",
		"~ constraint orders.orders_total_check: CHECK (total >= 0::numeric) → CHECK (total > 0::numeric)",
		"~ column tenant_notes.id: integer NOT NULL → bigint NOT NULL",
		"~ policy tenant_notes.tenant_notes_acme: FOR SELECT TO public USING (tenant = 'acme'::text) → " +
			"FOR SELECT TO public USING (tenant = 'globex'::text)",
	}
	if got := introspect.Diff(app, changed); !slices.Equal(got, want) {
		t.Errorf("Diff(app, changed) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	reversed := []string{
		"+ table audit_log",
		"- table coupons",
		"~ view customer_order_totals: " + innerJoin + " → " + leftJoin,
		"~ column customers.active: boolean NOT NULL DEFAULT false → boolean NOT NULL DEFAULT true",
		"~ column customers.name: text → text NOT NULL",
		"+ index customers.customers_lower_name_idx",
		"- index events.events_kind_idx",
		"- column orders.note",
		"~ constraint orders.orders_total_check: CHECK (total > 0::numeric) → CHECK (total >= 0::numeric)",
		"~ column tenant_notes.id: bigint NOT NULL → integer NOT NULL",
		"~ policy tenant_notes.tenant_notes_acme: FOR SELECT TO public USING (tenant = 'globex'::text) → " +
			"FOR SELECT TO public USING (tenant = 'acme'::text)",
	}
	if got := introspect.Diff(changed, app); !slices.Equal(got, reversed) {
		t.Errorf("Diff(changed, app) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(reversed, "\n"))
	}

	// Read again, as the superuser, who sees what ro_clean does there, the copy matches.
	if got := introspect.Diff(changed, structure(t, testdb.Querier(t, testdb.Superuser, name), "app")); len(got) != 0 {
		t.Errorf("the copy against itself:\n%s", strings.Join(got, "\n"))
	}
}
