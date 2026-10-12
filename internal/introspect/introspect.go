// Package introspect describes database objects from the system catalogs,
// rendered as compact text for agents.
package introspect

import (
	"context"
	"errors"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

var errNotImplemented = errors.New("not implemented")

// Schema is a non-system schema the current role can see.
type Schema struct {
	Name      string
	Relations int // tables, views, materialized views and foreign tables
	Comment   string
}

// SchemaList is the result of Schemas.
type SchemaList []Schema

// Render returns one line per schema.
func (l SchemaList) Render() string { return "" }

// Relation is a table-like object.
type Relation struct {
	Schema  string
	Name    string
	Kind    string // "table", "partitioned table", "view", "materialized view" or "foreign table"
	Rows    int64  // planner estimate (pg_class.reltuples); -1 when never analyzed
	Comment string
}

// RelationList is the result of Tables.
type RelationList []Relation

// Render returns one line per relation.
func (l RelationList) Render() string { return "" }

// Column is a column of a relation.
type Column struct {
	Name    string
	Type    string // format_type, e.g. "numeric(12,2)", "integer[]", "app.order_status"
	NotNull bool
	Default string // default expression; empty when none
	Comment string
}

// Index is an index of a table.
type Index struct {
	Name       string
	Definition string // pg_get_indexdef, e.g. "CREATE INDEX ... WHERE (status = 'pending')"
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
	RLSEnabled  bool
	Policies    []Policy
	Definition  string // query of a view or materialized view; empty for tables
}

// Render returns the description in a compact, DDL-like form.
func (t TableDetail) Render() string { return "" }

// Match is one hit of Search.
type Match struct {
	Schema string
	Table  string
	Column string // empty when the relation name itself matched
	Type   string // the column type, or the relation kind for a relation match
}

// MatchList is the result of Search.
type MatchList []Match

// Render returns one line per match.
func (l MatchList) Render() string { return "" }

// Schemas lists the non-system schemas the current role can see.
func Schemas(ctx context.Context, q engine.Querier) (SchemaList, error) {
	return nil, errNotImplemented
}

// Tables lists the relations in schema.
func Tables(ctx context.Context, q engine.Querier, schema string) (RelationList, error) {
	return nil, errNotImplemented
}

// Table describes the relation schema.name, or returns an error if the
// current role cannot see it.
func Table(ctx context.Context, q engine.Querier, schema, name string) (*TableDetail, error) {
	return nil, errNotImplemented
}

// Search finds relations and columns whose name contains pattern, ignoring
// case and treating pattern literally (no wildcards), and returns at most
// limit matches.
func Search(ctx context.Context, q engine.Querier, pattern string, limit int) (MatchList, error) {
	return nil, errNotImplemented
}

// Cache keeps rendered introspection output for a TTL, so repeated schema
// calls do not hit the catalogs. It is safe for concurrent use.
type Cache struct {
	ttl time.Duration
}

// NewCache returns an empty Cache whose entries expire after ttl.
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl}
}

// Get returns the value cached under key, calling load and caching its
// result when the entry is missing or expired. Errors are not cached.
func (c *Cache) Get(key string, load func() (string, error)) (string, error) {
	return "", errNotImplemented
}
