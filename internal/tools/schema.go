package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
)

const defaultSearchLimit = 50

const listLimit = 500

type schemaArgs struct {
	Connection string `json:"connection"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
}

func (h *Handlers) schema(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args schemaArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	if args.Schema == "" && args.Table != "" {
		args.Schema = h.cfg.Connections[args.Connection].DefaultSchema
		if args.Schema == "" {
			return "", errors.New("table needs schema: pass the schema that holds it")
		}
	}
	if err := errors.Join(checkName("schema", args.Schema), checkName("table", args.Table)); err != nil {
		return "", err
	}
	var err error
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	room := h.room(c)
	key := args.Connection + "\x00" + args.Schema + "\x00" + args.Table + "\x00" + strconv.Itoa(room)
	text, err := h.schemas.Get(key, func() (string, error) {
		return describe(ctx, h.engine.Querier(args.Connection), args.Schema, args.Table, room)
	})
	if err != nil {
		return "", &dbError{err}
	}
	return text, nil
}

func firstLines(text string, total, limit int) string {
	lines := strings.Split(text, "\n")
	total = max(total, len(lines))
	if len(lines) == total && len(text) <= limit {
		return text
	}
	note := func(shown int) string {
		return fmt.Sprintf("… %d of %d lines shown (output limit %d bytes): use pg_search to find names", shown, total, limit)
	}
	budget, n := limit-len(note(total)), 0
	for size := 0; n < len(lines) && size+len(lines[n])+1 <= budget; n++ {
		size += len(lines[n]) + 1
	}
	return strings.Join(append(lines[:n:n], note(n)), "\n")
}

func describe(ctx context.Context, q engine.Querier, schema, table string, limit int) (string, error) {
	switch {
	case schema == "":
		list, total, err := introspect.FirstSchemas(ctx, q, listLimit)
		return rendered(list, total, limit, err)
	case table == "":
		list, total, err := introspect.FirstTables(ctx, q, schema, listLimit)
		return rendered(list, total, limit, err)
	default:
		d, err := introspect.Table(ctx, q, schema, table)
		return rendered(d, 0, limit, err)
	}
}

func rendered[T interface{ Render() string }](v T, total, limit int, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return firstLines(v.Render(), total, limit), nil
}

type searchArgs struct {
	Connection string `json:"connection"`
	Pattern    string `json:"pattern"`
	Limit      int    `json:"limit"`
}

func (h *Handlers) search(ctx context.Context, raw json.RawMessage, c *call) (string, error) {
	var args searchArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := h.useConnection(args.Connection, c); err != nil {
		return "", err
	}
	if args.Pattern == "" {
		return "", errors.New("pattern must not be empty")
	}
	limit, cut, err := searchLimit(args.Limit, h.cfg.Defaults.MaxRows)
	if err != nil {
		return "", err
	}
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	matches, err := introspect.Search(ctx, h.engine.Querier(args.Connection), args.Pattern, min(limit, math.MaxInt-1)+1)
	if err != nil {
		return "", &dbError{err}
	}
	if len(matches) <= limit {
		cut = ""
	}
	matches = matches[:min(len(matches), limit)]
	c.rows, c.truncated = len(matches), cut != ""
	room := h.room(c)
	text := matches.Render()
	if cut != "" {
		text += "\n(truncated: " + cut + ")"
	}
	if len(text) <= room {
		return text, nil
	}
	return h.linesOutput("pg_search", args.Connection, "match", matches.Lines(), cut, room)
}

func searchLimit(limit, maxRows int) (int, string, error) {
	switch {
	case limit < 0:
		return 0, "", fmt.Errorf("limit must be at least 1, got %d", limit)
	case limit == 0:
		limit = defaultSearchLimit
	}
	if limit >= maxRows {
		return maxRows, "max_rows=" + strconv.Itoa(maxRows), nil
	}
	return limit, "limit=" + strconv.Itoa(limit), nil
}
