package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
)

const defaultDiffSchema = "public"

type diffSchemaArgs struct {
	Left   string `json:"left"`
	Right  string `json:"right"`
	Schema string `json:"schema"`
}

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
	if err := checkName("schema", args.Schema); err != nil {
		return "", err
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
	return h.diffText(args, len(left), lines, h.room(c))
}

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

func (h *Handlers) gatePair(ctx context.Context, left, right string, c *call) error {
	for _, conn := range []string{left, right} {
		notice, err := h.checks.gate(ctx, conn)
		if notice != "" {
			c.notice += conn + ": " + notice
		}
		if _, ok := errors.AsType[*blockedError](err); ok {
			return err
		}
		if err != nil {
			return &connError{conn: conn, err: err}
		}
	}
	return nil
}

func (h *Handlers) readStructure(ctx context.Context, conn, schema string) (introspect.SchemaStructure, error) {
	s, err := introspect.Structure(ctx, h.engine.Querier(conn), schema)
	if err != nil {
		return nil, &dbError{&connError{conn: conn, err: err}}
	}
	return s, nil
}

func (h *Handlers) diffText(args diffSchemaArgs, relations int, lines []string, room int) (string, error) {
	head := fmt.Sprintf("schema %s, %s → %s: ", args.Schema, args.Left, args.Right)
	if len(lines) == 0 {
		return head + "identical (" + plural(relations, "relation") + ")", nil
	}
	head += fmt.Sprintf("%s (- only in %s, + only in %s)", plural(len(lines), "difference"), args.Left, args.Right)
	text := head + "\n" + strings.Join(lines, "\n")
	if len(text) <= room {
		return text, nil
	}
	summary, err := h.linesOutput("pg_diff_schema", args.Left+"-"+args.Right, "difference", lines, "", room-len(head+"\n"))
	if err != nil {
		return "", err
	}
	return head + "\n" + summary, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
