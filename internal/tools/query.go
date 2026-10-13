package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

type queryArgs struct {
	Connection  string   `json:"connection"`
	Connections []string `json:"connections"`
	SQL         string   `json:"sql"`
	Params      []any    `json:"params"`
	Schema      string   `json:"schema"`
	Format      string   `json:"format"`
	MaxRows     int      `json:"max_rows"`
}

// query runs pg_query: the guard, the role check, the engine, then the
// result inline or spilled to a file. With connections, it fans out.
func (h *Handlers) query(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args queryArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	c.sql = args.SQL
	if args.Connections != nil {
		return h.fanout(ctx, args, c)
	}
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	format, err := h.checkQuery(args)
	if err != nil {
		return "", err
	}
	res, err := h.queryOn(ctx, args.Connection, args, c)
	if err != nil {
		return "", err
	}
	return h.spill.OutputWithin("pg_query", args.Connection, res, format, h.room(c))
}

// checkQuery checks the arguments of a pg_query call that hold for every
// connection, the guard last, and returns the format.
func (h *Handlers) checkQuery(args queryArgs) (render.Format, error) {
	format, err := h.format(args.Format)
	if err != nil {
		return "", err
	}
	if args.MaxRows < 0 {
		return "", fmt.Errorf("max_rows must be at least 1, got %d", args.MaxRows)
	}
	if err := checkParams(args.Params); err != nil {
		return "", err
	}
	return format, guard.Check(args.SQL)
}

// queryOn runs the statement of args, which checkQuery accepted, on conn:
// the search_path check, the role check, then the engine. It records the
// role check's notice and the rows in c.
func (h *Handlers) queryOn(ctx context.Context, conn string, args queryArgs, c *call) (*result.Result, error) {
	searchPath, err := h.searchPath(conn, args.Schema)
	if err != nil {
		return nil, err
	}
	if c.notice, err = h.checks.gate(ctx, conn); err != nil {
		return nil, err
	}
	res, err := h.engine.Query(ctx, conn, engine.Query{
		SQL:        args.SQL,
		Params:     args.Params,
		SearchPath: searchPath,
		MaxRows:    args.MaxRows,
	})
	if err != nil {
		return nil, &dbError{err}
	}
	c.rows, c.truncated = len(res.Rows), res.Truncated
	return res, nil
}

// checkParams accepts strings, numbers, booleans and nulls. Each is sent as
// text, with numbers exactly as written, and Postgres gives it the type the
// query implies. An array or object has no such text form.
func checkParams(params []any) error {
	for i, p := range params {
		switch p.(type) {
		case nil, string, bool, json.Number:
		default:
			return fmt.Errorf("params[%d] is an array or object: pass it as a string and cast it, e.g. '{1,2}' with $1::int[], or JSON with $1::jsonb", i+1)
		}
	}
	return nil
}

// format returns the format named name, or the configured one when name is
// empty.
func (h *Handlers) format(name string) (render.Format, error) {
	switch {
	case name == "":
		return render.Format(h.cfg.Defaults.Format), nil
	case slices.Contains(config.Formats, name):
		return render.Format(name), nil
	default:
		return "", fmt.Errorf("format must be one of %s, got %q", strings.Join(config.Formats, ", "), name)
	}
}

// searchPath returns schema, or the connection's default_schema when schema
// is empty. It refuses a schema longer than Postgres keeps a name and, with a
// *guard.Violation, one whose functions the guard denies: on the search_path,
// they could be called unqualified.
func (h *Handlers) searchPath(conn, schema string) (string, error) {
	if schema == "" {
		schema = h.cfg.Connections[conn].DefaultSchema
	}
	if err := checkName("schema", schema); err != nil {
		return "", err
	}
	return schema, guard.CheckSearchPath(schema)
}
