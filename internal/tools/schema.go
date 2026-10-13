package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
)

// defaultSearchLimit is how many matches pg_search returns without a limit.
const defaultSearchLimit = 50

// listLimit is how many schemas or relations pg_schema reads from the
// catalog for a listing: more lines than the inline limit shows in practice
// and, with their comments cut to 200 characters, a catalog result well
// within max_bytes however large the database.
const listLimit = 500

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
	if err := errors.Join(checkName("schema", args.Schema), checkName("table", args.Table)); err != nil {
		return "", err
	}
	var err error
	if c.notice, err = h.checks.gate(ctx, args.Connection); err != nil {
		return "", err
	}
	// The key holds the limit too: the role check's findings, which come with
	// the first call on a connection, leave less room.
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

// firstLines returns text when it fits in limit bytes and holds all total
// lines, or all of its own when total is lower. Otherwise it returns as many
// of its first lines as fit, then a line saying how many were shown.
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

// describe renders what the arguments ask for, cut to limit bytes: the
// schemas, the relations of schema, or the relation schema.table. A listing
// reads at most listLimit schemas or relations from the catalog, and the
// note of a cut listing counts them all.
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

// rendered renders v, which holds the first lines of total, cut to limit
// bytes.
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

// search runs pg_search: the matches, one per line, inline or spilled to a
// file whose summary says when the limit cut them.
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
	// One match past limit tells whether the limit cut the matches.
	matches, err := introspect.Search(ctx, h.engine.Querier(args.Connection), args.Pattern, min(limit, math.MaxInt-1)+1)
	if err != nil {
		return "", &dbError{err}
	}
	if len(matches) <= limit {
		cut = "" // every match came back
	}
	matches = matches[:min(len(matches), limit)]
	c.rows, c.truncated = len(matches), cut != ""
	room := h.room(c)
	if text := matches.Render(); len(text) <= room {
		return text, nil
	}
	return h.linesOutput("pg_search", args.Connection, "match", matches.Lines(), cut, room)
}

// searchLimit returns how many matches to return when asked for limit, the
// default when limit is 0 and never more than maxRows, and the limit that
// sets it, as a truncation reason: "limit=50", or "max_rows=1000" when the
// matches asked for are as many or more.
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
