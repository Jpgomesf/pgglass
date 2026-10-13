package introspect_test

import (
	_ "embed" // lab fixture SQL
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// labDB holds the lab fixture: the object kinds the shared app fixture lacks.
const labDB = "introspect_lab"

//go:embed testdata/lab.sql
var labSQL string

// loadLab creates labDB for the duration of the test.
func loadLab(t *testing.T) {
	t.Helper()
	testdb.CreateDatabase(t, labDB, labSQL)
}

func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

func describe(t *testing.T, q engine.Querier, schema, name string) *introspect.TableDetail {
	t.Helper()
	d, err := introspect.Table(t.Context(), q, schema, name)
	if err != nil {
		t.Fatalf("Table(%s, %s): %v", schema, name, err)
	}
	return d
}

// withoutSize checks that d has a size exactly when wantSize, then clears
// it: sizes depend on the server version, so tests compare the rest.
func withoutSize(t *testing.T, d *introspect.TableDetail, wantSize bool) *introspect.TableDetail {
	t.Helper()
	if (d.Size != "") != wantSize {
		t.Errorf("%s.%s size = %q, want a size: %v", d.Schema, d.Name, d.Size, wantSize)
	}
	d.Size = ""
	return d
}

func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want one containing %q", err, want)
	}
}

// everything is a listing limit past all the schemas and relations of the
// fixtures.
const everything = 100

func TestSchemasAsCleanRole(t *testing.T) {
	got, _, err := introspect.FirstSchemas(t.Context(), testdb.Querier(t, testdb.RoleClean, testdb.DBApp), everything)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "FirstSchemas", got, introspect.SchemaList{
		{Name: "app", Relations: 7, Comment: "Fixture application schema"},
		{Name: "public", Relations: 0, Comment: "standard public schema"},
	})
}

func TestSchemasExcludeSystemSchemas(t *testing.T) {
	// The superuser may use every schema, so only the exclusions keep these
	// out. A temporary table makes the session's pg_temp_N and pg_toast_temp_N.
	conn := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	if _, err := conn.Exec(t.Context(), "CREATE TEMP TABLE scratch (id int)"); err != nil {
		t.Fatal(err)
	}
	all := testdb.Value[string](t, conn, "SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_namespace")
	for _, system := range []string{"information_schema", "pg_catalog", "pg_toast", "pg_temp_", "pg_toast_temp_"} {
		if !strings.Contains(all, system) {
			t.Fatalf("no %s schema to exclude in %s", system, all)
		}
	}

	got, _, err := introspect.FirstSchemas(t.Context(), testdb.Querier(t, testdb.Superuser, testdb.DBApp), everything)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "FirstSchemas", got, introspect.SchemaList{
		{Name: "app", Relations: 7, Comment: "Fixture application schema"},
		{Name: "public", Relations: 0, Comment: "standard public schema"},
	})
}

func TestSchemasCountReadableRelationsWithoutPartitions(t *testing.T) {
	loadLab(t)

	// ro_clean cannot read lab.hidden, nor use schema locked.
	got, _, err := introspect.FirstSchemas(t.Context(), testdb.Querier(t, testdb.RoleClean, labDB), everything)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "FirstSchemas as ro_clean", got, introspect.SchemaList{
		{Name: "lab", Relations: 7, Comment: "Introspection lab"},
		{Name: "public", Relations: 0, Comment: "standard public schema"},
	})

	// The superuser reads everything; the two partitions still do not count.
	got, _, err = introspect.FirstSchemas(t.Context(), testdb.Querier(t, testdb.Superuser, labDB), everything)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "FirstSchemas as superuser", got, introspect.SchemaList{
		{Name: "lab", Relations: 8, Comment: "Introspection lab"},
		{Name: "locked", Relations: 1},
		{Name: "public", Relations: 0, Comment: "standard public schema"},
	})
}

func TestTablesListsAppRelations(t *testing.T) {
	got, _, err := introspect.FirstTables(t.Context(), testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", everything)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range got {
		if hasSize := r.Kind != "view"; (r.Size != "") != hasSize {
			t.Errorf("%s size = %q, want a size: %v", r.Name, r.Size, hasSize)
		}
		got[i].Size = ""
	}
	equal(t, "FirstTables(app)", got, introspect.RelationList{
		{Schema: "app", Name: "audit_log", Kind: "table", Rows: 50},
		{Schema: "app", Name: "customer_order_totals", Kind: "view", Rows: -1},
		{Schema: "app", Name: "customers", Kind: "table", Rows: 50, Comment: "People who place orders"},
		{Schema: "app", Name: "daily_event_counts", Kind: "materialized view", Rows: 56},
		{Schema: "app", Name: "events", Kind: "table", Rows: 20000},
		{Schema: "app", Name: "orders", Kind: "table", Rows: 50},
		{Schema: "app", Name: "tenant_notes", Kind: "table", Rows: 50, RLS: true},
	})
}

func TestTablesListsEveryKindButPartitions(t *testing.T) {
	loadLab(t)
	got, _, err := introspect.FirstTables(t.Context(), testdb.Querier(t, testdb.RoleClean, labDB), "lab", everything)
	if err != nil {
		t.Fatal(err)
	}
	sizes := make(map[string]string, len(got))
	for i, r := range got {
		sizes[r.Name] = r.Size
		got[i].Size = ""
	}
	// Not listed: the partitions measurements_2024 (readable) and
	// measurements_2025, and lab.hidden, which ro_clean cannot read.
	equal(t, "FirstTables(lab)", got, introspect.RelationList{
		{Schema: "lab", Name: "Mixed Case", Kind: "table", Rows: 0},
		{Schema: "lab", Name: "child", Kind: "table", Rows: 0, RLS: true, Comment: "Line items.\nSecond line of the comment."},
		{Schema: "lab", Name: "measurements", Kind: "partitioned table", Rows: 600},
		{Schema: "lab", Name: "parent", Kind: "table", Rows: 0},
		{Schema: "lab", Name: "remote_items", Kind: "foreign table", Rows: -1},
		{Schema: "lab", Name: "secrets", Kind: "table", Rows: 0},
		{Schema: "lab", Name: "wide", Kind: "view", Rows: -1},
	})

	partitions := testdb.Value[string](t, testdb.Conn(t, testdb.Superuser, labDB), `SELECT pg_size_pretty(
		pg_total_relation_size('lab.measurements_2024') + pg_total_relation_size('lab.measurements_2025'))`)
	if sizes["measurements"] != partitions {
		t.Errorf("partitioned table size = %q, want its partitions' %q", sizes["measurements"], partitions)
	}
	if sizes["remote_items"] != "" || sizes["wide"] != "" || sizes["child"] == "" {
		t.Errorf("sizes = %v, want none for the foreign table and the view only", sizes)
	}
}

func TestTablesRejectsUnusableSchemas(t *testing.T) {
	loadLab(t)
	q := testdb.Querier(t, testdb.RoleClean, labDB)
	for _, schema := range []string{"locked", "nope"} {
		got, _, err := introspect.FirstTables(t.Context(), q, schema, everything)
		wantErr(t, err, fmt.Sprintf("schema %q does not exist or the current role lacks USAGE on it", schema))
		if got != nil {
			t.Errorf("FirstTables(%s) = %v, want nil", schema, got)
		}
	}
}

func TestTableDescribesOrders(t *testing.T) {
	d := describe(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "orders")
	equal(t, "Table(app, orders)", *withoutSize(t, d, true), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "app", Name: "orders", Kind: "table", Rows: 50},
		Columns: []introspect.Column{
			{Name: "id", Type: "bigint", NotNull: true, Generated: "GENERATED ALWAYS AS IDENTITY"},
			{Name: "customer_id", Type: "uuid", NotNull: true},
			{Name: "status", Type: "app.order_status", NotNull: true, Default: "'pending'::app.order_status",
				EnumValues: []string{"pending", "paid", "shipped", "cancelled"}},
			{Name: "total", Type: "numeric(12,2)", NotNull: true},
			{Name: "quantities", Type: "integer[]", NotNull: true},
			{Name: "metadata", Type: "jsonb"},
			{Name: "fulfillment", Type: "interval"},
			{Name: "receipt", Type: "bytea"},
			{Name: "placed_at", Type: "timestamp with time zone", NotNull: true},
		},
		Indexes: []introspect.Index{
			{Name: "orders_pkey", Definition: "CREATE UNIQUE INDEX orders_pkey ON app.orders USING btree (id)", Primary: true, Unique: true},
			{Name: "orders_customer_id_idx", Definition: "CREATE INDEX orders_customer_id_idx ON app.orders USING btree (customer_id)"},
			{Name: "orders_pending_placed_at_idx", Definition: "CREATE INDEX orders_pending_placed_at_idx ON app.orders USING btree (placed_at) WHERE status = 'pending'::app.order_status"},
		},
		Constraints: []introspect.Constraint{
			{Name: "orders_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_customer_id_fkey", Kind: "foreign key", Definition: "FOREIGN KEY (customer_id) REFERENCES app.customers(id)"},
			{Name: "orders_total_check", Kind: "check", Definition: "CHECK (total >= 0::numeric)"},
		},
	})
}

func TestTableDescribesCustomers(t *testing.T) {
	d := describe(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "customers")
	equal(t, "Table(app, customers)", *withoutSize(t, d, true), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "app", Name: "customers", Kind: "table", Rows: 50, Comment: "People who place orders"},
		Columns: []introspect.Column{
			{Name: "id", Type: "uuid", NotNull: true},
			{Name: "email", Type: "text", NotNull: true, Comment: "Login email, unique per customer"},
			{Name: "name", Type: "text", NotNull: true},
			{Name: "active", Type: "boolean", NotNull: true, Default: "true"},
			{Name: "signup_date", Type: "date", NotNull: true},
			{Name: "created_at", Type: "timestamp with time zone", NotNull: true},
			{Name: "tags", Type: "text[]", NotNull: true, Default: "'{}'::text[]"},
			{Name: "last_ip", Type: "inet"},
			{Name: "profile", Type: "json"},
		},
		Indexes: []introspect.Index{
			{Name: "customers_pkey", Definition: "CREATE UNIQUE INDEX customers_pkey ON app.customers USING btree (id)", Primary: true, Unique: true},
			{Name: "customers_email_key", Definition: "CREATE UNIQUE INDEX customers_email_key ON app.customers USING btree (email)", Unique: true},
			{Name: "customers_lower_name_idx", Definition: "CREATE INDEX customers_lower_name_idx ON app.customers USING btree (lower(name))"},
		},
		Constraints: []introspect.Constraint{
			{Name: "customers_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
		},
	})
}

func TestTableDescribesRowLevelSecurity(t *testing.T) {
	d := describe(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "tenant_notes")
	if !d.RLS {
		t.Error("RLS = false, want true")
	}
	equal(t, "policies", d.Policies, []introspect.Policy{
		{Name: "tenant_notes_acme", Command: "SELECT", Permissive: true, Roles: []string{"public"}, Using: "tenant = 'acme'::text"},
	})

	d = describe(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "audit_log")
	if d.RLS || d.Policies != nil {
		t.Errorf("audit_log: RLS = %v, policies %v, want none", d.RLS, d.Policies)
	}
}

func TestTableDescribesLabChild(t *testing.T) {
	loadLab(t)
	d := describe(t, testdb.Querier(t, testdb.RoleClean, labDB), "lab", "child")
	moods := []string{"happy", "it's complicated", "sad"}
	equal(t, "Table(lab, child)", *withoutSize(t, d, true), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "lab", Name: "child", Kind: "table", Rows: 0, RLS: true,
			Comment: "Line items.\nSecond line of the comment."},
		Columns: []introspect.Column{
			{Name: "id", Type: "bigint", NotNull: true, Generated: "GENERATED BY DEFAULT AS IDENTITY"},
			{Name: "parent_id", Type: "integer", NotNull: true},
			{Name: "price", Type: "numeric(10,2)", NotNull: true},
			{Name: "qty", Type: "integer", NotNull: true, Default: "1"},
			{Name: "total", Type: "numeric", Generated: "GENERATED ALWAYS AS (price * qty::numeric) STORED"},
			{Name: "mood", Type: "lab.mood", EnumValues: moods, Comment: "How it went"},
			{Name: "moods", Type: "lab.mood[]", NotNull: true, Default: "'{}'::lab.mood[]", EnumValues: moods},
		},
		Indexes: []introspect.Index{
			{Name: "child_pkey", Definition: "CREATE UNIQUE INDEX child_pkey ON lab.child USING btree (id)", Primary: true, Unique: true},
			{Name: "child_parent_mood_key", Definition: "CREATE UNIQUE INDEX child_parent_mood_key ON lab.child USING btree (parent_id, mood)", Unique: true},
		},
		Constraints: []introspect.Constraint{
			{Name: "child_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
			{Name: "child_parent_id_fkey", Kind: "foreign key", Definition: "FOREIGN KEY (parent_id) REFERENCES lab.parent(id) ON DELETE CASCADE"},
			{Name: "child_parent_mood_key", Kind: "unique", Definition: "UNIQUE (parent_id, mood)"},
			{Name: "child_qty_positive", Kind: "check", Definition: "CHECK (qty > 0)"},
		},
		Policies: []introspect.Policy{
			{Name: "child_guard", Command: "ALL", Roles: []string{"public"}, Using: "price >= 0::numeric", WithCheck: "price < 1000::numeric"},
			{Name: "child_insert", Command: "INSERT", Permissive: true, Roles: []string{"public"}, WithCheck: "qty < 100"},
			{Name: "child_read", Command: "SELECT", Permissive: true, Roles: []string{"ro_clean", "ro_leaky"}, Using: "qty > 0"},
		},
		// The foreign key's internal triggers are left out.
		Triggers: []string{"child_audit", "child_touch"},
	})
}

func TestTableDescribesExclusionConstraints(t *testing.T) {
	loadLab(t)
	d := describe(t, testdb.Querier(t, testdb.RoleClean, labDB), "lab", "parent")
	equal(t, "constraints", d.Constraints, []introspect.Constraint{
		{Name: "parent_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
		{Name: "parent_active_excl", Kind: "exclusion", Definition: "EXCLUDE USING gist (active WITH &&)"},
		{Name: "parent_name_key", Kind: "unique", Definition: "UNIQUE (name)"},
	})
	equal(t, "indexes", d.Indexes, []introspect.Index{
		{Name: "parent_pkey", Definition: "CREATE UNIQUE INDEX parent_pkey ON lab.parent USING btree (id)", Primary: true, Unique: true},
		{Name: "parent_active_excl", Definition: "CREATE INDEX parent_active_excl ON lab.parent USING gist (active)"},
		{Name: "parent_name_key", Definition: "CREATE UNIQUE INDEX parent_name_key ON lab.parent USING btree (name)", Unique: true},
	})
}

func TestTableDescribesViews(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)

	d := describe(t, q, "app", "customer_order_totals")
	equal(t, "Table(app, customer_order_totals)", *withoutSize(t, d, false), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "app", Name: "customer_order_totals", Kind: "view", Rows: -1},
		Columns: []introspect.Column{
			{Name: "customer_id", Type: "uuid"},
			{Name: "email", Type: "text"},
			{Name: "order_count", Type: "bigint"},
			{Name: "total_spent", Type: "numeric"},
		},
		Definition: " SELECT c.id AS customer_id,\n    c.email,\n    count(o.id) AS order_count,\n" +
			"    COALESCE(sum(o.total), 0::numeric) AS total_spent\n   FROM app.customers c\n" +
			"     LEFT JOIN app.orders o ON o.customer_id = c.id\n  GROUP BY c.id, c.email;",
	})

	// Postgres 15 deparses this one with qualified column names, so only
	// version-independent parts are checked.
	d = describe(t, q, "app", "daily_event_counts")
	if d.Kind != "materialized view" || d.Rows != 56 || d.Size == "" || len(d.Columns) != 3 {
		t.Errorf("daily_event_counts = %+v", d.Relation)
	}
	for _, part := range []string{"SELECT date_trunc('day'::text, ", "count(*) AS events", "FROM app.events", "GROUP BY "} {
		if !strings.Contains(d.Definition, part) {
			t.Errorf("definition %q lacks %q", d.Definition, part)
		}
	}
}

func TestTableDescribesPartitionsAndForeignTables(t *testing.T) {
	loadLab(t)
	q := testdb.Querier(t, testdb.RoleClean, labDB)
	columns := []introspect.Column{
		{Name: "id", Type: "integer", NotNull: true},
		{Name: "taken_on", Type: "date", NotNull: true},
		{Name: "value", Type: "double precision"},
	}

	d := describe(t, q, "lab", "measurements")
	equal(t, "partitioned table", *withoutSize(t, d, true), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "lab", Name: "measurements", Kind: "partitioned table", Rows: 600},
		Columns:  columns,
	})

	// A partition is not listed, but it is described when the role can read it.
	d = describe(t, q, "lab", "measurements_2024")
	equal(t, "partition", *withoutSize(t, d, true), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "lab", Name: "measurements_2024", Kind: "table", Rows: 365},
		Columns:  columns,
	})

	d = describe(t, q, "lab", "remote_items")
	equal(t, "foreign table", *withoutSize(t, d, false), introspect.TableDetail{
		Relation: introspect.Relation{Schema: "lab", Name: "remote_items", Kind: "foreign table", Rows: -1},
		Columns:  []introspect.Column{{Name: "id", Type: "integer"}, {Name: "label", Type: "text"}},
	})
}

func TestTableShowsOnlyWhatTheRoleCanRead(t *testing.T) {
	loadLab(t)
	q := testdb.Querier(t, testdb.RoleClean, labDB)

	// ro_clean may SELECT only id and label: token stays hidden.
	d := describe(t, q, "lab", "secrets")
	equal(t, "secrets columns", d.Columns, []introspect.Column{
		{Name: "id", Type: "integer", NotNull: true},
		{Name: "label", Type: "text"},
	})

	d = describe(t, q, "lab", "Mixed Case")
	equal(t, "Mixed Case columns", d.Columns, []introspect.Column{{Name: "Weird Col", Type: "integer"}})

	for _, tc := range []struct{ schema, name string }{
		{"lab", "hidden"},            // no privilege at all
		{"lab", "measurements_2025"}, // a partition ro_clean cannot read
		{"locked", "vault"},          // SELECT granted, but no USAGE on the schema
		{"lab", "mixed case"},        // names are exact
		{"lab", "child_pkey"},        // an index is not table-like
		{"app", "orders"},            // another database
		{"nope", "nope"},
	} {
		got, err := introspect.Table(t.Context(), q, tc.schema, tc.name)
		wantErr(t, err, fmt.Sprintf("relation %q.%q does not exist or the current role cannot read it", tc.schema, tc.name))
		if got != nil {
			t.Errorf("Table(%s, %s) = %+v, want nil", tc.schema, tc.name, got)
		}
	}
}

func TestTableRenders(t *testing.T) {
	loadLab(t)
	for _, tc := range []struct {
		database, schema, name, want string
	}{
		{testdb.DBApp, "app", "orders", `app.orders: table, ~50 rows, SIZE
  id bigint NOT NULL GENERATED ALWAYS AS IDENTITY
  customer_id uuid NOT NULL
  status app.order_status NOT NULL DEFAULT 'pending'::app.order_status -- enum: 'pending', 'paid', 'shipped', 'cancelled'
  total numeric(12,2) NOT NULL
  quantities integer[] NOT NULL
  metadata jsonb
  fulfillment interval
  receipt bytea
  placed_at timestamp with time zone NOT NULL
constraints:
  orders_pkey PRIMARY KEY (id)
  orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES app.customers(id)
  orders_total_check CHECK (total >= 0::numeric)
indexes:
  CREATE INDEX orders_customer_id_idx ON app.orders USING btree (customer_id)
  CREATE INDEX orders_pending_placed_at_idx ON app.orders USING btree (placed_at) WHERE status = 'pending'::app.order_status`},
		{labDB, "lab", "child", `lab.child: table, ~0 rows, SIZE, RLS -- Line items. Second line of the comment.
  id bigint NOT NULL GENERATED BY DEFAULT AS IDENTITY
  parent_id integer NOT NULL
  price numeric(10,2) NOT NULL
  qty integer NOT NULL DEFAULT 1
  total numeric GENERATED ALWAYS AS (price * qty::numeric) STORED
  mood lab.mood -- How it went; enum: 'happy', 'it''s complicated', 'sad'
  moods lab.mood[] NOT NULL DEFAULT '{}'::lab.mood[] -- enum: 'happy', 'it''s complicated', 'sad'
constraints:
  child_pkey PRIMARY KEY (id)
  child_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES lab.parent(id) ON DELETE CASCADE
  child_parent_mood_key UNIQUE (parent_id, mood)
  child_qty_positive CHECK (qty > 0)
policies:
  child_guard AS RESTRICTIVE FOR ALL TO public USING (price >= 0::numeric) WITH CHECK (price < 1000::numeric)
  child_insert FOR INSERT TO public WITH CHECK (qty < 100)
  child_read FOR SELECT TO ro_clean, ro_leaky USING (qty > 0)
triggers: child_audit, child_touch`},
		// Every index implements a constraint, so there is no indexes section.
		{labDB, "lab", "parent", `lab.parent: table, ~0 rows, SIZE
  id integer NOT NULL
  name text NOT NULL
  active tstzrange
constraints:
  parent_pkey PRIMARY KEY (id)
  parent_active_excl EXCLUDE USING gist (active WITH &&)
  parent_name_key UNIQUE (name)`},
		{testdb.DBApp, "app", "customer_order_totals", `app.customer_order_totals: view
  customer_id uuid
  email text
  order_count bigint
  total_spent numeric
definition:
 SELECT c.id AS customer_id,
    c.email,
    count(o.id) AS order_count,
    COALESCE(sum(o.total), 0::numeric) AS total_spent
   FROM app.customers c
     LEFT JOIN app.orders o ON o.customer_id = c.id
  GROUP BY c.id, c.email;`},
	} {
		d := describe(t, testdb.Querier(t, testdb.RoleClean, tc.database), tc.schema, tc.name)
		got := d.Render()
		if d.Size != "" {
			got = strings.Replace(got, d.Size, "SIZE", 1)
		}
		if got != tc.want {
			t.Errorf("%s.%s Render() =\n%s\nwant\n%s", tc.schema, tc.name, got, tc.want)
		}
	}
}

func TestTableRenderCapsViewDefinition(t *testing.T) {
	loadLab(t)
	d := describe(t, testdb.Querier(t, testdb.RoleClean, labDB), "lab", "wide")
	if len(d.Columns) != 150 || !strings.Contains(d.Definition, "150 AS wide_column_150") {
		t.Fatalf("wide: %d columns, definition %q", len(d.Columns), d.Definition)
	}
	out := d.Render()
	note := fmt.Sprintf("\n-- truncated: 2000 of %d characters shown", utf8.RuneCountInString(d.Definition))
	if !strings.HasSuffix(out, note) || strings.Contains(out, "AS wide_column_150") {
		t.Errorf("Render() does not end with %q or shows the whole definition:\n%s", note, out)
	}
}

func TestSearchFindsRelationsColumnsAndFunctions(t *testing.T) {
	loadLab(t)
	q := testdb.Querier(t, testdb.RoleClean, labDB)
	for _, tc := range []struct {
		pattern string
		want    introspect.MatchList
	}{
		{"PARENT", introspect.MatchList{ // any case; relations before columns
			{Schema: "lab", Table: "parent", Type: "table"},
			{Schema: "lab", Table: "child", Column: "parent_id", Type: "integer"},
		}},
		{"total", introspect.MatchList{ // columns before functions; overloads by arguments
			{Schema: "lab", Table: "child", Column: "total", Type: "numeric"},
			{Schema: "lab", Table: "child_total(p_parent integer)", Type: "function"},
			{Schema: "lab", Table: "child_total(p_parent integer, p_mood lab.mood)", Type: "function"},
		}},
		{"measurements", introspect.MatchList{ // not the partitions
			{Schema: "lab", Table: "measurements", Type: "partitioned table"},
		}},
		{"item", introspect.MatchList{
			{Schema: "lab", Table: "remote_items", Type: "foreign table"},
		}},
		{"weird", introspect.MatchList{
			{Schema: "lab", Table: "Mixed Case", Column: "Weird Col", Type: "integer"},
		}},
		{"%", nil},      // literal: no name contains a percent sign
		{`\`, nil},      // nor a backslash
		{"hidden", nil}, // ro_clean cannot read lab.hidden
		{"token", nil},  // nor the column secrets.token
		{"vault", nil},  // nor use schema locked
		{"touch", nil},  // nor execute lab.touch()
		{"pg_class", nil},
		{"relname", nil}, // system catalogs are not searched
	} {
		got, err := introspect.Search(t.Context(), q, tc.pattern, 10)
		if err != nil {
			t.Fatalf("Search(%q): %v", tc.pattern, err)
		}
		if len(got) == 0 && tc.want == nil {
			continue
		}
		equal(t, fmt.Sprintf("Search(%q)", tc.pattern), got, tc.want)
	}
}

func TestSearchTreatsUnderscoreLiterally(t *testing.T) {
	loadLab(t)
	got, err := introspect.Search(t.Context(), testdb.Querier(t, testdb.RoleClean, labDB), "_", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// remote_items, parent_id, taken_on, the view's 150 wide_column_N and
	// the two child_total functions.
	if len(got) != 155 {
		t.Errorf("got %d matches, want 155", len(got))
	}
	for _, m := range got {
		name := m.Table
		if m.Column != "" {
			name = m.Column
		}
		if !strings.Contains(name, "_") {
			t.Errorf("match %+v has no underscore", m)
		}
	}
}

func TestSearchKeepsTheFirstLimitMatches(t *testing.T) {
	q := testdb.Querier(t, testdb.RoleClean, testdb.DBApp)
	all := introspect.MatchList{
		{Schema: "app", Table: "customer_order_totals", Type: "view"},
		{Schema: "app", Table: "orders", Type: "table"},
		{Schema: "app", Table: "customer_order_totals", Column: "order_count", Type: "bigint"},
	}
	for limit := 1; limit <= 4; limit++ {
		got, err := introspect.Search(t.Context(), q, "order", limit)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, fmt.Sprintf("Search(order, %d)", limit), got, all[:min(limit, len(all))])
	}
}

func TestSearchShowsOnlyExecutableFunctions(t *testing.T) {
	// dblink's functions are executable by ro_leaky, not by ro_clean.
	got, err := introspect.Search(t.Context(), testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "dblink_connect", 10)
	if err != nil || len(got) != 0 {
		t.Errorf("as ro_clean: %v, %v; want no matches", got, err)
	}
	got, err = introspect.Search(t.Context(), testdb.Querier(t, testdb.RoleLeaky, testdb.DBApp), "dblink_connect", 10)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "as ro_leaky", got, introspect.MatchList{
		{Schema: "public", Table: "dblink_connect(text)", Type: "function"},
		{Schema: "public", Table: "dblink_connect(text, text)", Type: "function"},
		{Schema: "public", Table: "dblink_connect_u(text)", Type: "function"},
		{Schema: "public", Table: "dblink_connect_u(text, text)", Type: "function"},
	})
}
