package tools

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/health"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

type healthArgs struct {
	Connection string `json:"connection"`
}

// health runs pg_health: the role check, then the health sections, whose
// lines are returned like a query result, inline or spilled to a file.
func (h *Handlers) health(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args healthArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	var err error
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	lines, err := health.Check(ctx, h.engine.Querier(args.Connection))
	if err != nil {
		return "", &dbError{err}
	}
	res := healthResult(lines)
	c.rows = len(res.Rows)
	return h.spill.Output("pg_health", args.Connection, res, render.Format(h.cfg.Defaults.Format))
}

// healthResult returns lines as a result with two text columns, section and
// detail.
func healthResult(lines []health.Line) *result.Result {
	res := &result.Result{Columns: []result.Column{
		{Name: "section", TypeOID: pgtype.TextOID, TypeName: "text"},
		{Name: "detail", TypeOID: pgtype.TextOID, TypeName: "text"},
	}}
	for _, l := range lines {
		section, detail := l.Section, l.Detail
		res.Rows = append(res.Rows, []*string{&section, &detail})
	}
	return res
}
