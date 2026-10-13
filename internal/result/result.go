// Package result holds a query result as Postgres prints it, so every type
// (uuid, numeric, jsonb, arrays, ...) keeps its exact text form.
package result

import "github.com/jackc/pgx/v5/pgtype"

// Column describes one result column. Names may repeat (SELECT 1 AS a, 2 AS a).
type Column struct {
	Name     string
	TypeOID  uint32 // pg_type.oid of the column type
	TypeName string // pg_type.typname, e.g. "int4", "numeric", "_text", "order_status"
}

// Result is the outcome of one query.
type Result struct {
	Columns []Column
	// Rows hold values in Postgres TEXT output format, one slice per row in
	// column order; nil means NULL.
	Rows [][]*string
	// Truncated reports that rows were dropped to respect a limit, which
	// TruncatedReason names (e.g. "max_rows=1000").
	Truncated       bool
	TruncatedReason string
}

// Text returns a result with no rows whose columns, named names, hold text:
// for output a tool builds itself, rendered and spilled like a query's.
func Text(names ...string) *Result {
	cols := make([]Column, len(names))
	for i, name := range names {
		cols[i] = Column{Name: name, TypeOID: pgtype.TextOID, TypeName: "text"}
	}
	return &Result{Columns: cols}
}

// AddRow appends a row of values, none of them NULL. The row holds copies,
// so values may be reused.
func (r *Result) AddRow(values ...string) {
	row := make([]*string, len(values))
	for i, v := range values {
		row[i] = &v // v is a new variable in each iteration
	}
	r.Rows = append(r.Rows, row)
}
