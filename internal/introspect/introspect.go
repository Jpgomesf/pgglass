// Package introspect describes database objects from the system catalogs,
// rendered as compact text for agents.
//
// Every query reads pg_catalog through an engine.Querier, so it runs in a
// read-only transaction, and pins search_path to pg_catalog: names outside
// pg_catalog come back schema-qualified, and objects in user schemas cannot
// shadow the catalog functions the queries call. Each query returns one JSON
// value that decodes straight into the types below.
//
// The queries cut what a description cannot show, so that no one comment or
// definition, however long, takes that value past max_bytes: a comment to its
// first 200 characters, a view's query to 2000, any other expression to
// 10,000, longer than a line of a 10 KB description, and an enum to its first
// 2500 labels. Search and Profile cut nothing, since every match and value
// counts whole: they read their value with engine.WholeValueMaxBytes in place
// of a smaller max_bytes.
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
	Relations int
	Comment   string
}

// SchemaList is the result of FirstSchemas.
type SchemaList []Schema

// Relation is a table-like object.
type Relation struct {
	Schema string
	Name   string
	Kind   string
	Rows   int64
	// Size is the pretty total size, TOAST, indexes and partitions included,
	// e.g. "48 kB"; empty for views and foreign tables.
	Size    string
	RLS     bool
	Comment string
}

// RelationList is the result of FirstTables.
type RelationList []Relation

// Column is a column of a relation. NotNull is set only by a validated
// constraint, not a NOT VALID one (PostgreSQL 18).
type Column struct {
	Name    string
	Type    string
	NotNull bool
	Default string
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
	Definition string
	Primary    bool
	Unique     bool
}

// Constraint is a primary key, unique, check, foreign key or exclusion constraint.
type Constraint struct {
	Name       string
	Kind       string
	Definition string
}

// Policy is a row-level security policy.
type Policy struct {
	Name       string
	Command    string
	Permissive bool
	Roles      []string
	Using      string
	WithCheck  string
}

// TableDetail is the full description of one relation, returned by Table.
type TableDetail struct {
	Relation
	Columns     []Column
	Indexes     []Index
	Constraints []Constraint
	Policies    []Policy
	Triggers    []string
	Definition  string
}

// Match is one hit of Search.
type Match struct {
	Schema string
	// Table is the relation name or, for a function match, the function name
	// and identity arguments, e.g. "order_total(customer uuid)".
	Table  string
	Column string
	// Type is the column type, the relation kind for a relation match, or
	// "function", "aggregate", "window function" or "procedure".
	Type string
}

// MatchList is the result of Search.
type MatchList []Match

var errNoRows = errors.New("no rows")

var errTooLarge = errors.New("catalog result exceeds the configured limits")

// ErrNotFound is wrapped by the error of Table for a relation that does not
// exist or that the current role cannot read.
var ErrNotFound = errors.New("does not exist or the current role cannot read it")

// FirstSchemas lists the first limit non-system schemas the role can see, by
// name, and returns how many there are in all.
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

// FirstTables lists the first limit relations in schema the role can read, by
// name, partitions aside, with their total; an unusable schema is an error.
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

// Table describes the relation schema.name, partitions included, or returns an
// error wrapping ErrNotFound if the role cannot see it. Names match exactly.
func Table(ctx context.Context, q engine.Querier, schema, name string) (*TableDetail, error) {
	var t TableDetail
	err := queryJSON(ctx, q, &t, tableSQL, schema, name)
	if errors.Is(err, errNoRows) {
		return nil, fmt.Errorf("relation %q.%q %w", schema, name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("describe relation %q.%q: %w", schema, name, err)
	}
	return &t, nil
}

// Search returns at most limit relations, then columns, then functions whose
// name contains pattern, literally and ignoring case.
func Search(ctx context.Context, q engine.Querier, pattern string, limit int) (MatchList, error) {
	if pattern == "" {
		return nil, errors.New("search pattern is empty")
	}
	if limit < 1 {
		return nil, fmt.Errorf("search limit must be at least 1, got %d", limit)
	}
	var list MatchList
	if err := queryWholeJSON(ctx, q, &list, searchSQL, likePattern(pattern), limit); err != nil {
		return nil, fmt.Errorf("search %q: %w", pattern, err)
	}
	return list, nil
}

func queryJSON(ctx context.Context, q engine.Querier, dst any, sql string, params ...any) error {
	return decodeValue(ctx, q, dst, engine.Query{SQL: sql, Params: params})
}

func queryWholeJSON(ctx context.Context, q engine.Querier, dst any, sql string, params ...any) error {
	return decodeValue(ctx, q, dst, engine.Query{SQL: sql, Params: params, MaxBytes: engine.WholeValueMaxBytes})
}

func decodeValue(ctx context.Context, q engine.Querier, dst any, query engine.Query) error {
	query.SearchPath = "pg_catalog"
	res, err := q.Query(ctx, query)
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
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likePattern(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}
