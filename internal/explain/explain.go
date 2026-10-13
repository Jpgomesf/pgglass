// Package explain summarizes the plan of one SELECT for an agent, in a few
// lines instead of EXPLAIN's output: the total cost, the nodes that cost the
// most by themselves, and flags for what is worth a look.
//
// The plan comes from EXPLAIN (FORMAT JSON, VERBOSE), with ANALYZE and
// BUFFERS on request, run through an engine.Querier: in a READ ONLY
// transaction that is always rolled back, under the statement timeout, so
// ANALYZE runs the query but keeps nothing it changes. VERBOSE names the
// schema of each relation, by which its size is looked up.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// reltuplesSQL returns, as JSON, pg_class.reltuples of each relation that
// the schemas in $1 and the names in $2 name, pairwise: -1 when it was never
// analyzed. It runs with search_path pinned to pg_catalog.
const reltuplesSQL = `SELECT coalesce(json_agg(json_build_object('Schema', n.nspname, 'Name', c.relname, 'Rows', c.reltuples)), '[]')
FROM unnest($1::text[], $2::text[]) AS r (nspname, relname)
JOIN pg_namespace n ON n.nspname = r.nspname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = r.relname`

// Statement is the EXPLAIN of one SELECT that the guard accepts.
type Statement struct {
	sql    string // EXPLAIN (...) followed by the SELECT
	offset int    // characters of sql before the SELECT
}

// New returns the Statement that explains sql: EXPLAIN (FORMAT JSON,
// VERBOSE), with ANALYZE and BUFFERS when analyze is set.
//
// sql must pass guard.Check, as pg_query's SQL does, and of what that allows
// only a SELECT can be explained. EXPLAIN takes no parameter for its query,
// so sql follows the options as text, and guard.Check checks the whole
// statement as well. Every rejection is a *guard.Violation.
func New(sql string, analyze bool) (Statement, error) {
	if err := guard.Check(sql); err != nil {
		return Statement{}, err
	}
	prefix := "EXPLAIN (FORMAT JSON, VERBOSE) "
	if analyze {
		prefix = "EXPLAIN (FORMAT JSON, VERBOSE, ANALYZE, BUFFERS) "
	}
	s := Statement{sql: prefix + sql, offset: len(prefix)} // ASCII: one byte per character
	if err := guard.Check(s.sql); err != nil {
		return Statement{}, &guard.Violation{Reason: "only a SELECT can be explained, not EXPLAIN or SHOW"}
	}
	return s, nil
}

// SQL returns the EXPLAIN statement.
func (s Statement) SQL() string { return s.sql }

// Run runs s through q with searchPath as its search_path (the connection's
// own when empty), looks up the size of every relation the plan scans
// sequentially, and returns the summary. Errors raised by Postgres are
// *engine.QueryError, positioned in the SELECT as New was given it.
func (s Statement) Run(ctx context.Context, q engine.Querier, searchPath string) (string, error) {
	res, err := q.Query(ctx, engine.Query{SQL: s.sql, SearchPath: searchPath})
	if err != nil {
		var qe *engine.QueryError
		if errors.As(err, &qe) && qe.Position > s.offset {
			qe.Position -= s.offset
		}
		return "", err
	}
	doc, err := onlyValue(res, "the plan")
	if err != nil {
		return "", err
	}
	p, err := parse(doc)
	if err != nil {
		return "", err
	}
	sizes, err := relationSizes(ctx, q, p.seqScans())
	if err != nil {
		return "", err
	}
	return p.summary(sizes), nil
}

// relationSizes returns pg_class.reltuples of each of rels.
func relationSizes(ctx context.Context, q engine.Querier, rels []relation) (map[relation]float64, error) {
	sizes := make(map[relation]float64, len(rels))
	if len(rels) == 0 {
		return sizes, nil
	}
	schemas, names := make([]string, len(rels)), make([]string, len(rels))
	for i, r := range rels {
		schemas[i], names[i] = r.schema, r.name
	}
	res, err := q.Query(ctx, engine.Query{SQL: reltuplesSQL, Params: []any{schemas, names}, SearchPath: "pg_catalog"})
	if err != nil {
		return nil, err
	}
	doc, err := onlyValue(res, "relation sizes")
	if err != nil {
		return nil, err
	}
	var found []struct {
		Schema, Name string
		Rows         float64
	}
	if err := json.Unmarshal([]byte(doc), &found); err != nil {
		return nil, fmt.Errorf("read relation sizes: %w", err)
	}
	for _, f := range found {
		sizes[relation{schema: f.Schema, name: f.Name}] = f.Rows
	}
	return sizes, nil
}

// onlyValue returns the value of res, which holds one row of one value.
func onlyValue(res *result.Result, what string) (string, error) {
	switch {
	case res.Truncated:
		return "", fmt.Errorf("read %s: it exceeds the configured limits (%s)", what, res.TruncatedReason)
	case len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] == nil:
		return "", errors.New("read " + what + ": no value came back")
	}
	return *res.Rows[0][0], nil
}
