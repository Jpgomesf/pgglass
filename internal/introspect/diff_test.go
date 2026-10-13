package introspect_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/introspect"
)

// defs are the definitions of one kind of object in a relation, by name.
type defs = map[string]string

func TestDiff(t *testing.T) {
	orders := introspect.RelationStructure{
		Kind:        "table",
		RLS:         true,
		Columns:     defs{"id": "bigint NOT NULL", "total": "numeric(12,2) NOT NULL"},
		Constraints: defs{"orders_pkey": "PRIMARY KEY (id)"},
		Indexes:     defs{"orders_total_idx": "CREATE INDEX orders_total_idx ON app.orders USING btree (total)"},
		Policies:    defs{"orders_read": "FOR SELECT TO public USING (true)"},
	}
	totals := introspect.RelationStructure{Kind: "view", Definition: " SELECT count(*) AS n\n   FROM app.orders;", Columns: defs{"n": "bigint"}}

	for _, tc := range []struct {
		name        string
		left, right introspect.SchemaStructure
		want        []string
	}{
		{
			name:  "identical",
			left:  introspect.SchemaStructure{"orders": orders, "totals": totals},
			right: introspect.SchemaStructure{"orders": orders, "totals": totals},
		},
		{name: "both empty"},
		{
			name:  "no objects and empty maps are alike",
			left:  introspect.SchemaStructure{"t": {Kind: "table"}},
			right: introspect.SchemaStructure{"t": {Kind: "table", Columns: defs{}, Policies: defs{}}},
		},
		{
			// What is in a relation only one side has is not listed.
			name:  "relations only one side has",
			left:  introspect.SchemaStructure{"orders": orders, "totals": totals},
			right: introspect.SchemaStructure{"totals": totals, "daily": {Kind: "materialized view", Columns: defs{"day": "date"}}},
			want:  []string{"+ materialized view daily", "- table orders"},
		},
		{
			name:  "kind and row-level security",
			left:  introspect.SchemaStructure{"notes": {Kind: "table", RLS: true}, "totals": {Kind: "view"}},
			right: introspect.SchemaStructure{"notes": {Kind: "table"}, "totals": {Kind: "table", RLS: true}},
			want: []string{
				"~ relation notes: table with RLS → table",
				"~ relation totals: view → table with RLS",
			},
		},
		{
			// Queries show on one line, after the relation's kind and
			// before its columns.
			name: "queries of views and materialized views",
			left: introspect.SchemaStructure{
				"v":    {Kind: "view", Definition: " SELECT id,\n    name\n   FROM s.t;", Columns: defs{"id": "integer", "name": "text"}},
				"mv":   {Kind: "materialized view", Definition: " SELECT 1 AS n;", Columns: defs{"n": "integer"}},
				"same": {Kind: "view", Definition: " SELECT 1 AS n;", Columns: defs{"n": "integer"}},
			},
			right: introspect.SchemaStructure{
				"v": {Kind: "view", Definition: " SELECT id,\n    name\n   FROM s.t\n  WHERE id > 0;",
					Columns: defs{"id": "integer", "name": "character varying"}},
				"mv":   {Kind: "materialized view", Definition: " SELECT 2 AS n;", Columns: defs{"n": "integer"}},
				"same": {Kind: "view", Definition: " SELECT 1 AS n;", Columns: defs{"n": "integer"}},
			},
			want: []string{
				"~ materialized view mv: SELECT 1 AS n; → SELECT 2 AS n;",
				"~ view v: SELECT id, name FROM s.t; → SELECT id, name FROM s.t WHERE id > 0;",
				"~ column v.name: text → character varying",
			},
		},
		{
			// The kind says enough when only one side has a query; when both
			// do, the line takes the left side's kind.
			name:  "a view that changed kind",
			left:  introspect.SchemaStructure{"a": {Kind: "view", Definition: " SELECT 1 AS n;"}, "b": {Kind: "view", Definition: " SELECT 1 AS n;"}},
			right: introspect.SchemaStructure{"a": {Kind: "table"}, "b": {Kind: "materialized view", Definition: " SELECT 2 AS n;"}},
			want: []string{
				"~ relation a: view → table",
				"~ relation b: view → materialized view",
				"~ view b: SELECT 1 AS n; → SELECT 2 AS n;",
			},
		},
		{
			name: "columns",
			left: introspect.SchemaStructure{"t": {Kind: "table", Columns: defs{
				"a": "integer", "b": "text NOT NULL", "c": "text DEFAULT 'x'::text", "d": "date",
			}}},
			right: introspect.SchemaStructure{"t": {Kind: "table", Columns: defs{
				"a": "bigint", "c": "text DEFAULT 'y'::text", "d": "date", "e": "bigint GENERATED ALWAYS AS IDENTITY",
			}}},
			want: []string{
				"~ column t.a: integer → bigint",
				"- column t.b",
				"~ column t.c: text DEFAULT 'x'::text → text DEFAULT 'y'::text",
				"+ column t.e",
			},
		},
		{
			name: "constraints, indexes and policies",
			left: introspect.SchemaStructure{"t": {
				Kind:        "table",
				Constraints: defs{"t_check": "CHECK (n > 0)", "t_pkey": "PRIMARY KEY (id)"},
				Indexes:     defs{"t_n_idx": "CREATE INDEX t_n_idx ON s.t USING btree (n)"},
				Policies:    defs{"t_read": "FOR SELECT TO public USING (true)", "t_write": "FOR INSERT TO app WITH CHECK (true)"},
			}},
			right: introspect.SchemaStructure{"t": {
				Kind:        "table",
				Constraints: defs{"t_check": "CHECK (n >= 0)", "t_fkey": "FOREIGN KEY (id) REFERENCES s.u(id)"},
				Indexes:     defs{"t_m_idx": "CREATE INDEX t_m_idx ON s.t USING btree (m)", "t_n_idx": "CREATE INDEX t_n_idx ON s.t USING hash (n)"},
				Policies:    defs{"t_read": "AS RESTRICTIVE FOR SELECT TO public USING (true)"},
			}},
			want: []string{
				"~ constraint t.t_check: CHECK (n > 0) → CHECK (n >= 0)",
				"+ constraint t.t_fkey",
				"- constraint t.t_pkey",
				"+ index t.t_m_idx",
				"~ index t.t_n_idx: CREATE INDEX t_n_idx ON s.t USING btree (n) → CREATE INDEX t_n_idx ON s.t USING hash (n)",
				"~ policy t.t_read: FOR SELECT TO public USING (true) → AS RESTRICTIVE FOR SELECT TO public USING (true)",
				"- policy t.t_write",
			},
		},
		{
			// Relations by name in byte order, then in each: the relation,
			// its columns, constraints, indexes and policies.
			name: "order",
			left: introspect.SchemaStructure{
				"b": {Kind: "table", Columns: defs{"y": "text", "x": "integer"}, Indexes: defs{"b_x_idx": "CREATE INDEX b_x_idx ON s.b USING btree (x)"}},
				"B": {Kind: "table"},
				"a": {Kind: "view"},
			},
			right: introspect.SchemaStructure{
				"c": {Kind: "table"},
				"b": {Kind: "table", RLS: true, Columns: defs{"x": "bigint"}, Constraints: defs{"b_pkey": "PRIMARY KEY (x)"},
					Policies: defs{"b_all": "FOR ALL TO public USING (true)"}},
				"a": {Kind: "view"},
			},
			want: []string{
				"- table B",
				"~ relation b: table → table with RLS",
				"~ column b.x: integer → bigint",
				"- column b.y",
				"+ constraint b.b_pkey",
				"- index b.b_x_idx",
				"+ policy b.b_all",
				"+ table c",
			},
		},
		{
			name:  "definitions on several lines",
			left:  introspect.SchemaStructure{"t": {Kind: "table", Columns: defs{"k": "text DEFAULT CASE\n    WHEN true THEN 'a'::text\n    ELSE 'b'::text\nEND"}}},
			right: introspect.SchemaStructure{"t": {Kind: "table", Columns: defs{"k": "text"}}},
			want:  []string{"~ column t.k: text DEFAULT CASE WHEN true THEN 'a'::text ELSE 'b'::text END → text"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := introspect.Diff(tc.left, tc.right); !slices.Equal(got, tc.want) {
				t.Errorf("Diff =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
