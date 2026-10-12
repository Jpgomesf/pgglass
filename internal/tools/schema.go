package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
)

// defaultSearchLimit is how many matches pg_search returns without a limit.
const defaultSearchLimit = 50

type schemaArgs struct {
	Connection string `json:"connection"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
}

// schema runs pg_schema: the schemas, the relations of one schema, or one
// relation in detail, each cached and cut to the inline limit.
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
	var err error
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	key := args.Connection + "\x00" + args.Schema + "\x00" + args.Table
	text, err := h.schemas.Get(key, func() (string, error) {
		return describe(ctx, h.engine.Querier(args.Connection), args.Schema, args.Table)
	})
	if err != nil {
		return "", &dbError{err}
	}
	return firstLines(text, h.spill.InlineMaxBytes), nil
}

// firstLines returns text when it fits in limit bytes. Otherwise it returns
// as many of its first lines as fit, then a line saying how many were shown.
func firstLines(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	lines := strings.Split(text, "\n")
	note := func(shown int) string {
		return fmt.Sprintf("… %d of %d lines shown (output limit %d bytes): use pg_search to find names", shown, len(lines), limit)
	}
	budget, n := limit-len(note(len(lines))), 0
	for size := 0; n < len(lines) && size+len(lines[n])+1 <= budget; n++ {
		size += len(lines[n]) + 1
	}
	return strings.Join(append(lines[:n:n], note(n)), "\n")
}

// describe renders what the arguments ask for: the schemas, the relations of
// schema, or the relation schema.table.
func describe(ctx context.Context, q engine.Querier, schema, table string) (string, error) {
	switch {
	case schema == "":
		return rendered(introspect.Schemas(ctx, q))
	case table == "":
		return rendered(introspect.Tables(ctx, q, schema))
	default:
		return rendered(introspect.Table(ctx, q, schema, table))
	}
}

func rendered[T interface{ Render() string }](v T, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return v.Render(), nil
}

type searchArgs struct {
	Connection string `json:"connection"`
	Pattern    string `json:"pattern"`
	Limit      int    `json:"limit"`
}

// search runs pg_search.
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
	limit, err := searchLimit(args.Limit, h.cfg.Defaults.MaxRows)
	if err != nil {
		return "", err
	}
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	matches, err := introspect.Search(ctx, h.engine.Querier(args.Connection), args.Pattern, limit)
	if err != nil {
		return "", &dbError{err}
	}
	c.rows = len(matches)
	return matches.Render(), nil
}

// searchLimit returns how many matches to return when asked for limit: the
// default when limit is 0, and never more than maxRows.
func searchLimit(limit, maxRows int) (int, error) {
	switch {
	case limit < 0:
		return 0, fmt.Errorf("limit must be at least 1, got %d", limit)
	case limit == 0:
		return min(defaultSearchLimit, maxRows), nil
	default:
		return min(limit, maxRows), nil
	}
}
