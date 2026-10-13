package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
	"github.com/jpgomesf/mcp-postgres/internal/render"
)

type profileArgs struct {
	Connection string `json:"connection"`
	Table      string `json:"table"`
	Schema     string `json:"schema"`
	Column     string `json:"column"`
}

// profile runs pg_profile: the values of a table's columns, from pg_stats or
// from a sample, after a line that says which, inline or spilled to a file.
func (h *Handlers) profile(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args profileArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	schema, table, err := h.profileTarget(args)
	if err != nil {
		return "", err
	}
	// The statistics of a catalog the guard denies, such as pg_authid's, hold
	// its values.
	if err := guard.CheckRelation(table); err != nil {
		return "", err
	}
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	p, err := introspect.Profile(ctx, h.engine.Querier(args.Connection), schema, table, args.Column)
	if err != nil {
		return "", &dbError{err}
	}
	c.rows = len(p.Columns)
	// Whole: five uuids among the most common values alone take more than the
	// 200 characters a query's inline cells are cut to.
	out, err := h.spill.OutputWhole("pg_profile", args.Connection, p.Result(), render.Format(h.cfg.Defaults.Format))
	if err != nil {
		return "", err
	}
	return p.Summary() + "\n" + out, nil
}

// profileTarget returns the schema and name of the table a pg_profile call
// names: table as schema.name, or name in schema, or in the connection's
// default_schema.
func (h *Handlers) profileTarget(args profileArgs) (schema, table string, err error) {
	if args.Table == "" {
		return "", "", errors.New("table must not be empty")
	}
	schema, table = args.Schema, args.Table
	if schema == "" {
		var qualified bool
		if schema, table, qualified = strings.Cut(args.Table, "."); !qualified {
			schema, table = h.cfg.Connections[args.Connection].DefaultSchema, args.Table
		}
	}
	if schema == "" || table == "" {
		return "", "", errors.New("table needs a schema: pass schema.table, or the schema that holds it")
	}
	return schema, table, nil
}
