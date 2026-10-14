package tools

import (
	"context"
	"encoding/json"

	"github.com/jpgomesf/pgglass/internal/health"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

type healthArgs struct {
	Connection string `json:"connection"`
}

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
	return h.spill.OutputWhole("pg_health", args.Connection, res, render.Format(h.cfg.Defaults.Format), h.room(c))
}

func healthResult(lines []health.Line) *result.Result {
	res := result.Text("section", "detail")
	for _, l := range lines {
		res.AddRow(l.Section, l.Detail)
	}
	return res
}
