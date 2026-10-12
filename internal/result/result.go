// Package result holds a query result as Postgres prints it, so every type
// (uuid, numeric, jsonb, arrays, ...) keeps its exact text form.
package result

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
