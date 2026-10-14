// Package explain summarizes the plan of one SELECT for an agent, in a few
// lines instead of EXPLAIN's output: the total cost, the nodes that cost the
// most by themselves, and flags for what is worth a look.
//
// The plan comes from EXPLAIN (FORMAT JSON, VERBOSE), with ANALYZE and
// BUFFERS on request, run through an engine.Querier: in a READ ONLY
// transaction that is always rolled back, under the statement timeout, so
// ANALYZE runs the query but keeps nothing it changes. VERBOSE names the
// schema of each relation, by which its size is looked up. The plan, which
// grows with every partition a query scans, is read whole to be summarized,
// up to 16 MiB or max_bytes, whichever is larger.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/result"
)

const reltuplesSQL = `SELECT coalesce(json_agg(json_build_object('Schema', n.nspname, 'Name', c.relname, 'Rows', c.reltuples)), '[]')
FROM unnest($1::text[], $2::text[]) AS r (nspname, relname)
JOIN pg_namespace n ON n.nspname = r.nspname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = r.relname`

// Statement is the EXPLAIN of one SELECT that the guard accepts.
type Statement struct {
	sql    string
	offset int
}

// New returns the JSON EXPLAIN of sql, a SELECT the guard accepts, with ANALYZE
// and BUFFERS when analyze is set. Every rejection is a *guard.Violation.
func New(sql string, analyze bool) (Statement, error) {
	if err := guard.Check(sql); err != nil {
		return Statement{}, err
	}
	prefix := "EXPLAIN (FORMAT JSON, VERBOSE) "
	if analyze {
		prefix = "EXPLAIN (FORMAT JSON, VERBOSE, ANALYZE, BUFFERS) "
	}
	s := Statement{sql: prefix + sql, offset: len(prefix)}
	if err := guard.Check(s.sql); err != nil {
		return Statement{}, &guard.Violation{Reason: "only a SELECT can be explained, not EXPLAIN or SHOW"}
	}
	return s, nil
}

// Run runs s through q under searchPath, the connection's own when empty, and
// returns the summary. Error positions point into the SELECT New was given.
func (s Statement) Run(ctx context.Context, q engine.Querier, searchPath string) (string, error) {
	res, err := q.Query(ctx, engine.Query{SQL: s.sql, SearchPath: searchPath, MaxBytes: engine.WholeValueMaxBytes})
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

func relationSizes(ctx context.Context, q engine.Querier, rels []relation) (map[relation]float64, error) {
	sizes := make(map[relation]float64, len(rels))
	if len(rels) == 0 {
		return sizes, nil
	}
	schemas, names := make([]string, len(rels)), make([]string, len(rels))
	for i, r := range rels {
		schemas[i], names[i] = r.schema, r.name
	}
	res, err := q.Query(ctx, engine.Query{SQL: reltuplesSQL, Params: []any{schemas, names}, SearchPath: "pg_catalog", MaxBytes: engine.WholeValueMaxBytes})
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

func onlyValue(res *result.Result, what string) (string, error) {
	switch {
	case res.Truncated:
		return "", fmt.Errorf("read %s: it exceeds %s", what, res.TruncatedReason)
	case len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] == nil:
		return "", errors.New("read " + what + ": no value came back")
	}
	return *res.Rows[0][0], nil
}
