package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// defaultDiffSchema is the schema pg_diff_schema compares when given none.
const defaultDiffSchema = "public"

type diffSchemaArgs struct {
	Left   string `json:"left"`
	Right  string `json:"right"`
	Schema string `json:"schema"`
}

// diffSchema runs pg_diff_schema: the role check of both connections, one
// catalog query on each, then their differences, inline or spilled to a
// file.
func (h *Handlers) diffSchema(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args diffSchemaArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := h.useConnectionPair(args.Left, args.Right, c); err != nil {
		return "", err
	}
	if args.Schema == "" {
		args.Schema = defaultDiffSchema
	}
	if err := h.gatePair(ctx, args.Left, args.Right, c); err != nil {
		return "", err
	}
	left, err := h.readStructure(ctx, args.Left, args.Schema)
	if err != nil {
		return "", err
	}
	right, err := h.readStructure(ctx, args.Right, args.Schema)
	if err != nil {
		return "", err
	}
	lines := introspect.Diff(left, right)
	c.rows = len(lines)
	return h.diffText(args, len(left), lines)
}

// useConnectionPair checks that left and right name two configured
// connections and records both for the audit line, as "left,right".
func (h *Handlers) useConnectionPair(left, right string, c *call) error {
	for _, name := range []string{left, right} {
		if _, ok := h.cfg.Connections[name]; !ok {
			return engine.UnknownConnection(name, h.cfg.Connections)
		}
	}
	if left == right {
		return errors.New("left and right must name two different connections")
	}
	c.connection = left + "," + right
	return nil
}

// gatePair runs the role check of left, then of right. What each reports
// starts the result, under its connection's name.
func (h *Handlers) gatePair(ctx context.Context, left, right string, c *call) error {
	for _, conn := range []string{left, right} {
		notice, err := h.checks.gate(ctx, conn)
		if notice != "" {
			c.notice += conn + ": " + notice
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// readStructure reads the structure of schema on the connection conn.
func (h *Handlers) readStructure(ctx context.Context, conn, schema string) (introspect.SchemaStructure, error) {
	s, err := introspect.Structure(ctx, h.engine.Querier(conn), schema)
	if err != nil {
		return nil, &dbError{fmt.Errorf("%s: %w", conn, err)}
	}
	return s, nil
}

// diffText renders the result of pg_diff_schema. Its first line says what
// was compared and either that it is identical, counting its relations, or
// how many differences follow and which side is which. Past the inline
// limit, the differences go to a spill file instead, one per row, and their
// summary follows the first line.
func (h *Handlers) diffText(args diffSchemaArgs, relations int, lines []string) (string, error) {
	head := fmt.Sprintf("schema %s, %s → %s: ", args.Schema, args.Left, args.Right)
	if len(lines) == 0 {
		return head + "identical (" + plural(relations, "relation") + ")", nil
	}
	head += fmt.Sprintf("%s (- only in %s, + only in %s)", plural(len(lines), "difference"), args.Left, args.Right)
	text := head + "\n" + strings.Join(lines, "\n")
	if len(text) <= h.spill.InlineMaxBytes {
		return text, nil
	}
	res := &result.Result{Columns: []result.Column{{Name: "difference", TypeName: "text"}}}
	for i := range lines {
		res.Rows = append(res.Rows, []*string{&lines[i]})
	}
	summary, err := h.spill.Output("pg_diff_schema", args.Left+"-"+args.Right, res, render.TSV)
	if err != nil {
		return "", err
	}
	return head + "\n" + summary, nil
}

// plural returns "1 <noun>" or "<n> <noun>s".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
