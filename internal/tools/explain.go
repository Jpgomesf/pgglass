package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jpgomesf/pgglass/internal/explain"
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
	if err == nil && args.Analyze {
		err = h.confirm(c, "pg_explain with analyze", []string{args.Connection}, args.SQL, nil)
	}
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
	room := h.room(c)
	if len(summary) <= room {
		return summary, nil
	}
	return h.linesOutput("pg_explain", args.Connection, "line", strings.Split(summary, "\n"), "", room)
}
