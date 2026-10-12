package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/explain"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

type explainArgs struct {
	Connection string `json:"connection"`
	SQL        string `json:"sql"`
	Analyze    bool   `json:"analyze"`
	Schema     string `json:"schema"`
}

// explain runs pg_explain: the guard, as for pg_query, the role check, then
// the plan of the SELECT, summarized inline or spilled to a file.
func (h *Handlers) explain(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args explainArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	c.sql = args.SQL
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	stmt, err := explain.New(args.SQL, args.Analyze)
	if err != nil {
		return "", err
	}
	searchPath, err := h.searchPath(args.Connection, args.Schema)
	if err != nil {
		return "", err
	}
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	summary, err := stmt.Run(ctx, h.engine.Querier(args.Connection), searchPath)
	if err != nil {
		return "", &dbError{err}
	}
	if len(summary) <= h.spill.InlineMaxBytes {
		return summary, nil
	}
	return h.spill.Output("pg_explain", args.Connection, lines(summary), render.TSV)
}

// lines holds text as a result of one text column, one row per line, for
// the spill store.
func lines(text string) *result.Result {
	res := &result.Result{Columns: []result.Column{{Name: "line", TypeName: "text"}}}
	for _, line := range strings.Split(text, "\n") {
		res.Rows = append(res.Rows, []*string{&line})
	}
	return res
}
