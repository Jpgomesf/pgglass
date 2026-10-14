package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/render"
)

type profileArgs struct {
	Connection string `json:"connection"`
	Table      string `json:"table"`
	Schema     string `json:"schema"`
	Column     string `json:"column"`
}

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
	summary := p.Summary()
	out, err := h.spill.OutputWhole("pg_profile", args.Connection, p.Result(), render.Format(h.cfg.Defaults.Format),
		h.room(c)-len(summary+"\n"))
	if err != nil {
		return "", err
	}
	return summary + "\n" + out, nil
}

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
	if err := errors.Join(checkName("schema", schema), checkName("table", table), checkName("column", args.Column)); err != nil {
		return "", "", err
	}
	return schema, table, nil
}
