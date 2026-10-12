package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/render"
)

type queryArgs struct {
	Connection string `json:"connection"`
	SQL        string `json:"sql"`
	Params     []any  `json:"params"`
	Schema     string `json:"schema"`
	Format     string `json:"format"`
	MaxRows    int    `json:"max_rows"`
}

// query runs pg_query: the guard, the role check, the engine, then the
// result inline or spilled to a file.
func (h *Handlers) query(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args queryArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	c.sql = args.SQL
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
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
	if err := guard.Check(args.SQL); err != nil {
		return "", err
	}
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	res, err := h.engine.Query(ctx, args.Connection, engine.Query{
		SQL:        args.SQL,
		Params:     args.Params,
		SearchPath: h.searchPath(args.Connection, args.Schema),
		MaxRows:    args.MaxRows,
	})
	if err != nil {
		return "", &dbError{err}
	}
	c.rows, c.truncated = len(res.Rows), res.Truncated
	return h.spill.Output("pg_query", args.Connection, res, format)
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
	switch name {
	case "":
		return render.Format(h.cfg.Defaults.Format), nil
	case config.FormatTSV, config.FormatJSON, config.FormatMarkdown:
		return render.Format(name), nil
	default:
		return "", fmt.Errorf("format must be one of tsv, json, markdown, got %q", name)
	}
}

// searchPath returns schema, or the connection's default_schema when schema
// is empty.
func (h *Handlers) searchPath(conn, schema string) string {
	if schema != "" {
		return schema
	}
	return h.cfg.Connections[conn].DefaultSchema
}
