package introspect_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/introspect"
)

type renderer interface{ Render() string }

func wantRender(t *testing.T, name string, r renderer, want string) {
	t.Helper()
	if got := r.Render(); got != want {
		t.Errorf("%s Render() =\n%s\nwant\n%s", name, got, want)
	}
}

func TestRenderEmptyLists(t *testing.T) {
	wantRender(t, "SchemaList", introspect.SchemaList{}, "no schemas the current role can use")
	wantRender(t, "RelationList", introspect.RelationList(nil), "no relations the current role can read")
	wantRender(t, "MatchList", introspect.MatchList{}, "no matches")
}

func TestRenderSchemaList(t *testing.T) {
	wantRender(t, "SchemaList", introspect.SchemaList{
		{Name: "app", Relations: 7, Comment: "Main\n  application schema"},
		{Name: "audit", Relations: 1},
		{Name: "empty"},
	}, "app: 7 relations -- Main application schema\n"+
		"audit: 1 relation\n"+
		"empty: 0 relations")
}

func TestRenderRelationList(t *testing.T) {
	wantRender(t, "RelationList", introspect.RelationList{
		{Schema: "app", Name: "orders", Kind: "table", Rows: 50, Size: "64 kB", RLS: true, Comment: "Orders\r\nplaced online"},
		{Schema: "app", Name: "settings", Kind: "table", Rows: 1, Size: "16 kB"},
		{Schema: "app", Name: "fresh", Kind: "table", Rows: -1, Size: "0 bytes"},
		{Schema: "app", Name: "emptied", Kind: "table", Rows: 0, Size: "8192 bytes"},
		{Schema: "app", Name: "totals", Kind: "view", Rows: -1},
		{Schema: "app", Name: "remote", Kind: "foreign table", Rows: 10},
	}, "app.orders: table, ~50 rows, 64 kB, RLS -- Orders placed online\n"+
		"app.settings: table, ~1 row, 16 kB\n"+
		"app.fresh: table, 0 bytes\n"+
		"app.emptied: table, ~0 rows, 8192 bytes\n"+
		"app.totals: view\n"+
		"app.remote: foreign table, ~10 rows")
}

func TestRenderMatchList(t *testing.T) {
	wantRender(t, "MatchList", introspect.MatchList{
		{Schema: "app", Table: "orders", Type: "table"},
		{Schema: "app", Table: "orders", Column: "total", Type: "numeric(12,2)"},
		{Schema: "app", Table: "order_total(customer uuid)", Type: "function"},
	}, "app.orders: table\n"+
		"app.orders.total: numeric(12,2)\n"+
		"app.order_total(customer uuid): function")
}

// Lines gives each match one line, even when a quoted name holds a newline,
// so a spilled pg_search writes one row per match.
func TestMatchListLines(t *testing.T) {
	got := introspect.MatchList{
		{Schema: "app", Table: "orders", Column: "two\nlines", Type: "text"},
		{Schema: "app", Table: "orders", Type: "table"},
	}.Lines()
	if want := []string{"app.orders.two\nlines: text", "app.orders: table"}; !slices.Equal(got, want) {
		t.Errorf("Lines() = %q, want %q", got, want)
	}
	if got := introspect.MatchList(nil).Lines(); len(got) != 0 {
		t.Errorf("Lines() of no matches = %q, want none", got)
	}
}

func TestRenderTableDetail(t *testing.T) {
	d := introspect.TableDetail{
		Relation: introspect.Relation{Schema: "app", Name: "orders", Kind: "table", Rows: 50, Size: "64 kB", RLS: true, Comment: "Customer orders"},
		Columns: []introspect.Column{
			{Name: "id", Type: "bigint", NotNull: true, Generated: "GENERATED ALWAYS AS IDENTITY"},
			{Name: "status", Type: "app.order_status", NotNull: true, Default: "'pending'::app.order_status",
				EnumValues: []string{"pending", "it's late"}, Comment: "Where the order\n  stands"},
			{Name: "tags", Type: "app.order_status[]", EnumValues: []string{"pending", "it's late"}},
			{Name: "total", Type: "numeric", Generated: "GENERATED ALWAYS AS (price * qty) STORED"},
			{Name: "kind", Type: "text", Default: "CASE\n    WHEN true THEN 'a'::text\n    ELSE 'b'::text\nEND"},
			{Name: "note", Type: "text", Comment: "Free text"},
		},
		Constraints: []introspect.Constraint{
			{Name: "orders_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_note_key", Kind: "unique", Definition: "UNIQUE (note)"},
			{Name: "orders_total_check", Kind: "check", Definition: "CHECK (total >= 0)"},
		},
		Indexes: []introspect.Index{
			{Name: "orders_pkey", Definition: "CREATE UNIQUE INDEX orders_pkey ON app.orders USING btree (id)", Primary: true, Unique: true},
			{Name: "orders_note_key", Definition: "CREATE UNIQUE INDEX orders_note_key ON app.orders USING btree (note)", Unique: true},
			{Name: "orders_status_idx", Definition: "CREATE INDEX orders_status_idx ON app.orders USING btree (status) WHERE status = 'pending'"},
		},
		Policies: []introspect.Policy{
			{Name: "staff_all", Command: "ALL", Permissive: true, Roles: []string{"admin", "staff"}, Using: "true"},
			{Name: "positive", Command: "INSERT", Roles: []string{"public"}, WithCheck: "total >= 0"},
			{Name: "tenant", Command: "UPDATE", Roles: []string{"public"}, Using: "tenant = 'a'", WithCheck: "tenant = 'a'"},
		},
		Triggers: []string{"orders_audit", "orders_touch"},
	}
	wantRender(t, "TableDetail", d, `app.orders: table, ~50 rows, 64 kB, RLS -- Customer orders
  id bigint NOT NULL GENERATED ALWAYS AS IDENTITY
  status app.order_status NOT NULL DEFAULT 'pending'::app.order_status -- Where the order stands; enum: 'pending', 'it''s late'
  tags app.order_status[] -- enum: 'pending', 'it''s late'
  total numeric GENERATED ALWAYS AS (price * qty) STORED
  kind text DEFAULT CASE WHEN true THEN 'a'::text ELSE 'b'::text END
  note text -- Free text
constraints:
  orders_pkey PRIMARY KEY (id)
  orders_note_key UNIQUE (note)
  orders_total_check CHECK (total >= 0)
indexes:
  CREATE INDEX orders_status_idx ON app.orders USING btree (status) WHERE status = 'pending'
policies:
  staff_all FOR ALL TO admin, staff USING (true)
  positive AS RESTRICTIVE FOR INSERT TO public WITH CHECK (total >= 0)
  tenant AS RESTRICTIVE FOR UPDATE TO public USING (tenant = 'a') WITH CHECK (tenant = 'a')
triggers: orders_audit, orders_touch`)
}

func TestRenderTableDetailOmitsEmptySections(t *testing.T) {
	wantRender(t, "bare table", introspect.TableDetail{
		Relation: introspect.Relation{Schema: "app", Name: "bare", Kind: "table", Rows: -1},
		Columns:  []introspect.Column{{Name: "id", Type: "integer"}},
	}, "app.bare: table\n  id integer")

	wantRender(t, "constraint indexes only", introspect.TableDetail{
		Relation:    introspect.Relation{Schema: "app", Name: "keyed", Kind: "table", Rows: 3},
		Constraints: []introspect.Constraint{{Name: "keyed_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"}},
		Indexes:     []introspect.Index{{Name: "keyed_pkey", Definition: "CREATE UNIQUE INDEX keyed_pkey ON app.keyed USING btree (id)", Primary: true, Unique: true}},
	}, "app.keyed: table, ~3 rows\nconstraints:\n  keyed_pkey PRIMARY KEY (id)")
}

func TestRenderTableDetailDefinition(t *testing.T) {
	view := introspect.TableDetail{
		Relation:   introspect.Relation{Schema: "app", Name: "v", Kind: "view", Rows: -1},
		Columns:    []introspect.Column{{Name: "a", Type: "integer"}},
		Definition: " SELECT 1 AS a;\n",
	}
	wantRender(t, "view", view, "app.v: view\n  a integer\ndefinition:\n SELECT 1 AS a;")

	view.Definition = strings.Repeat("é", 2500)
	want := "app.v: view\n  a integer\ndefinition:\n" + strings.Repeat("é", 2000) +
		"\n-- truncated: 2000 of 2500 characters shown"
	wantRender(t, "long view", view, want)

	view.Definition = strings.Repeat("é", 2000)
	wantRender(t, "view at the cap", view, "app.v: view\n  a integer\ndefinition:\n"+strings.Repeat("é", 2000))
}
