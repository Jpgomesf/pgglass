// Package health checks a server from its statistics views and catalogs, for
// agents: connections, long-running queries and transactions, blocked
// sessions, cache hit ratio, dead rows, unused indexes, transaction ID
// wraparound and replication lag, each as a few compact lines.
//
// Each section is one query run through an engine.Querier, so in a read-only
// transaction, with search_path pinned to pg_catalog. A section whose query
// the role may not run says so, and the other sections still run. No query
// reads the text of other sessions' queries, which can hold secrets.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

// longRunning is how long a query must have been running, or a transaction
// open, to be listed as long-running.
const longRunning = time.Minute

// insufficientPrivilege is the SQLSTATE of "permission denied".
const insufficientPrivilege = "42501"

// Line is one line of the report.
type Line struct {
	Section string // e.g. "connections"
	Detail  string
}

// Check runs every section through q and returns their lines, in order. A
// section whose query fails in Postgres says so in its line, as not visible
// to the role when it lacks the privilege; Check itself fails only when q
// does otherwise, for example when the server is unreachable.
func Check(ctx context.Context, q engine.Querier) ([]Line, error) {
	return check(ctx, q, sections(longRunning))
}

// section is one part of the report: a query that returns one JSON value,
// and how to render that value.
type section struct {
	name   string
	sql    string
	params []any
	render func(data []byte, v viewer) ([]string, error)
}

// sections returns the sections of the report, in order. Queries running for
// longer than minDuration, and the sessions in a transaction open for longer,
// are long-running.
func sections(minDuration time.Duration) []section {
	return []section{
		{"connections", connectionsSQL, nil, decoded(connectionLines)},
		{"long_running", longRunningSQL, []any{int(minDuration.Seconds())}, decoded(longRunningLines)},
		{"blocked", blockedSQL, nil, decoded(blockedLines)},
		{"cache_hit", cacheHitSQL, nil, decoded(cacheHitLines)},
		{"dead_tuples", deadTuplesSQL, nil, decoded(deadTupleLines)},
		{"unused_indexes", unusedIndexesSQL, nil, decoded(unusedIndexLines)},
		{"xid_wraparound", wraparoundSQL, nil, decoded(wraparoundLines)},
		{"replication", replicationSQL, nil, decoded(replicationLines)},
	}
}

func check(ctx context.Context, q engine.Querier, all []section) ([]Line, error) {
	var v viewer
	data, err := queryValue(ctx, q, viewerSQL)
	if err == nil {
		err = decode(data, &v)
	}
	if err != nil {
		return nil, fmt.Errorf("health: current role: %w", err)
	}
	var lines []Line
	for _, s := range all {
		details, err := s.run(ctx, q, v)
		if err != nil {
			return nil, fmt.Errorf("health: %s: %w", s.name, err)
		}
		for _, d := range details {
			lines = append(lines, Line{Section: s.name, Detail: d})
		}
	}
	return lines, nil
}

// run returns the lines of s. When Postgres refuses its query for lack of
// privilege, the one line says the section is not visible to the role; when
// the query fails in Postgres otherwise, the line quotes the error.
func (s section) run(ctx context.Context, q engine.Querier, v viewer) ([]string, error) {
	data, err := queryValue(ctx, q, s.sql, s.params...)
	var pgErr *engine.QueryError
	switch {
	case errors.As(err, &pgErr) && pgErr.SQLState == insufficientPrivilege:
		return []string{"not visible to role " + v.Role}, nil
	case errors.As(err, &pgErr):
		return []string{"failed: " + pgErr.Error()}, nil
	case err != nil:
		return nil, err
	}
	return s.render(data, v)
}

// decoded returns a render function that decodes a JSON value into a T, then
// renders it with render.
func decoded[T any](render func(T, viewer) []string) func([]byte, viewer) ([]string, error) {
	return func(data []byte, v viewer) ([]string, error) {
		var value T
		if err := decode(data, &value); err != nil {
			return nil, err
		}
		return render(value, v), nil
	}
}

// queryValue runs sql, which returns at most one row holding one JSON value,
// and returns that value: null when no row or SQL NULL comes back.
func queryValue(ctx context.Context, q engine.Querier, sql string, params ...any) ([]byte, error) {
	res, err := q.Query(ctx, engine.Query{SQL: sql, Params: params, SearchPath: "pg_catalog"})
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("result exceeds the configured limits (%s)", res.TruncatedReason)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 || res.Rows[0][0] == nil {
		return []byte("null"), nil
	}
	return []byte(*res.Rows[0][0]), nil
}

// decode reads the JSON value data into dst. A key the SQL misspells fails
// loudly.
func decode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
