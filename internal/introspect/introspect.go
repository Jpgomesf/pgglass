// Package introspect describes database objects from the system catalogs,
// rendered as compact text for agents.
//
// Every query reads pg_catalog through an engine.Querier, so it runs in a
// read-only transaction, and pins search_path to pg_catalog: names outside
// pg_catalog come back schema-qualified, and objects in user schemas cannot
// shadow the catalog functions the queries call. Each query returns one JSON
// value that decodes straight into the types below.
//
// Only what the current role can read is described: schemas it has USAGE on,
// relations it can SELECT at least one column of, columns it can SELECT and
// functions it can EXECUTE. Partitions are reached through their parent, so
// listings and searches leave them out.
package introspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jpgomesf/pgglass/internal/engine"
)

// Schema is a non-system schema the current role can see.
type Schema struct {
	Name      string
	Relations int // tables, views, materialized views and foreign tables
	Comment   string
}

// SchemaList is the result of FirstSchemas.
type SchemaList []Schema

// Relation is a table-like object.
type Relation struct {
	Schema string
	Name   string
	Kind   string // "table", "partitioned table", "view", "materialized view" or "foreign table"
	Rows   int64  // planner estimate (pg_class.reltuples); -1 when never analyzed
	// Size is the pretty total size, TOAST, indexes and partitions included,
	// e.g. "48 kB"; empty for views and foreign tables.
	Size    string
	RLS     bool // row-level security is enabled
	Comment string
}

// RelationList is the result of FirstTables.
type RelationList []Relation

// Column is a column of a relation.
type Column struct {
	Name    string
	Type    string // format_type, e.g. "numeric(12,2)", "integer[]", "app.order_status"
	NotNull bool
	Default string // default expression; empty when none
	// Generated is the identity or generation clause, e.g. "GENERATED ALWAYS
	// AS IDENTITY" or "GENERATED ALWAYS AS (price * qty) STORED"; empty when none.
	Generated string
	// EnumValues are the labels, in order, of an enum type or of the element
	// type of an enum array.
	EnumValues []string
	Comment    string
}

// Index is an index of a table.
type Index struct {
	Name       string
	Definition string // pg_get_indexdef, e.g. "CREATE INDEX ... WHERE status = 'pending'::app.order_status"
	Primary    bool
	Unique     bool
}

// Constraint is a primary key, unique, check, foreign key or exclusion constraint.
type Constraint struct {
	Name       string
	Kind       string // "primary key", "unique", "check", "foreign key" or "exclusion"
	Definition string // pg_get_constraintdef, e.g. "FOREIGN KEY (customer_id) REFERENCES app.customers(id)"
}

// Policy is a row-level security policy.
type Policy struct {
	Name       string
	Command    string   // "ALL", "SELECT", "INSERT", "UPDATE" or "DELETE"
	Permissive bool     // false for RESTRICTIVE policies
	Roles      []string // "public" when the policy applies to everyone
	Using      string   // USING expression; empty when none
	WithCheck  string   // WITH CHECK expression; empty when none
}

// TableDetail is the full description of one relation, returned by Table.
type TableDetail struct {
	Relation
	Columns     []Column
	Indexes     []Index
	Constraints []Constraint
	Policies    []Policy
	Triggers    []string // user-defined triggers; internal constraint triggers are left out
	Definition  string   // query of a view or materialized view; empty for tables
}

// Match is one hit of Search.
type Match struct {
	Schema string
	// Table is the relation name or, for a function match, the function name
	// and identity arguments, e.g. "order_total(customer uuid)".
	Table  string
	Column string // empty when the relation or function name itself matched
	// Type is the column type, the relation kind for a relation match, or
	// "function", "aggregate", "window function" or "procedure".
	Type string
}

// MatchList is the result of Search.
type MatchList []Match

// errNoRows reports a catalog query that found nothing, such as a missing
// relation or one the role cannot read.
var errNoRows = errors.New("no rows")

// errTooLarge reports a catalog result that max_rows or max_bytes cut short.
var errTooLarge = errors.New("catalog result exceeds the configured limits")

// FirstSchemas lists the first limit of the non-system schemas the current
// role can see, by name, and returns how many there are in all, so the
// catalog result stays small however many schemas there are.
func FirstSchemas(ctx context.Context, q engine.Querier, limit int) (SchemaList, int, error) {
	if limit < 1 {
		return nil, 0, fmt.Errorf("limit must be at least 1, got %d", limit)
	}
	var page struct {
		Schemas SchemaList
		Total   int
	}
	if err := queryJSON(ctx, q, &page, schemasSQL, limit); err != nil {
		return nil, 0, fmt.Errorf("list schemas: %w", err)
	}
	return page.Schemas, page.Total, nil
}

// FirstTables lists the first limit of the relations in schema that the
// current role can read, by name, leaving out partitions, and returns how
// many there are in all, so the catalog result stays small however many
// relations the schema holds. It returns an error if the schema does not
// exist or the role lacks USAGE on it.
func FirstTables(ctx context.Context, q engine.Querier, schema string, limit int) (RelationList, int, error) {
	if limit < 1 {
		return nil, 0, fmt.Errorf("limit must be at least 1, got %d", limit)
	}
	var page struct {
		Relations RelationList
		Total     int
	}
	err := queryJSON(ctx, q, &page, tablesSQL, schema, limit)
	if errors.Is(err, errNoRows) {
		return nil, 0, fmt.Errorf("schema %q does not exist or the current role lacks USAGE on it", schema)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("list relations in schema %q: %w", schema, err)
	}
	return page.Relations, page.Total, nil
}

// Table describes the relation schema.name, or returns an error if the
// current role cannot see it. Names match exactly, as the catalog stores
// them (unquoted). Partitions can be described too.
func Table(ctx context.Context, q engine.Querier, schema, name string) (*TableDetail, error) {
	var t TableDetail
	err := queryJSON(ctx, q, &t, tableSQL, schema, name)
	if errors.Is(err, errNoRows) {
		return nil, fmt.Errorf("relation %q.%q does not exist or the current role cannot read it", schema, name)
	}
	if err != nil {
		return nil, fmt.Errorf("describe relation %q.%q: %w", schema, name, err)
	}
	return &t, nil
}

// Search finds relations and columns whose name contains pattern, ignoring
// case and treating pattern literally (no wildcards), and returns at most
// limit matches. Functions whose name contains pattern match too. Relations
// come first, then columns, then functions.
func Search(ctx context.Context, q engine.Querier, pattern string, limit int) (MatchList, error) {
	if pattern == "" {
		return nil, errors.New("search pattern is empty")
	}
	if limit < 1 {
		return nil, fmt.Errorf("search limit must be at least 1, got %d", limit)
	}
	var list MatchList
	if err := queryJSON(ctx, q, &list, searchSQL, likePattern(pattern), limit); err != nil {
		return nil, fmt.Errorf("search %q: %w", pattern, err)
	}
	return list, nil
}

// queryJSON runs sql, which returns at most one row holding one JSON value,
// and decodes that value into dst. It returns errNoRows when no row or NULL
// comes back.
func queryJSON(ctx context.Context, q engine.Querier, dst any, sql string, params ...any) error {
	res, err := q.Query(ctx, engine.Query{SQL: sql, Params: params, SearchPath: "pg_catalog"})
	if err != nil {
		return err
	}
	if res.Truncated {
		return fmt.Errorf("%w (%s)", errTooLarge, res.TruncatedReason)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 || res.Rows[0][0] == nil {
		return errNoRows
	}
	dec := json.NewDecoder(strings.NewReader(*res.Rows[0][0]))
	dec.DisallowUnknownFields() // a key the SQL misspells fails loudly
	return dec.Decode(dst)
}

// likeEscaper escapes the ILIKE wildcards and its escape character.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likePattern returns an ILIKE pattern that matches s literally anywhere.
func likePattern(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}
