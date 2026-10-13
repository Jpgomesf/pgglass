// Package tools implements the MCP tools: pg_list_connections, pg_schema,
// pg_search, pg_query, pg_explain, pg_profile, pg_diff_schema and pg_health.
//
// Every failure is a tool result with IsError set, never a protocol error,
// and every call writes one JSON audit line, or one per connection when
// pg_query fans out, that holds no SQL text, parameter or value.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// Handlers serves the tools for the connections of one config. It is safe
// for concurrent use.
type Handlers struct {
	cfg     config.Config
	engine  *engine.Manager
	spill   *spill.Store
	schemas *introspect.Cache
	checks  *roleChecks
	log     *slog.Logger
}

// New returns the Handlers for cfg, whose defaults are filled in, as
// config.Parse returns them. They query through eng, spill results too large
// to return to store, and write one JSON audit line per call, or per
// connection when pg_query fans out, to auditLog.
func New(cfg config.Config, eng *engine.Manager, store *spill.Store, auditLog io.Writer) *Handlers {
	return &Handlers{
		cfg:     cfg,
		engine:  eng,
		spill:   store,
		schemas: introspect.NewCache(introspect.DefaultCacheTTL),
		checks:  newRoleChecks(cfg.Defaults.RoleCheck, eng),
		log:     slog.New(slog.NewJSONHandler(auditLog, nil)),
	}
}

// NewServer returns an MCP server, named mcp-postgres, that exposes the tools
// of h.
func NewServer(h *Handlers, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-postgres", Version: version}, nil)
	s.AddTool(listConnectionsTool, h.handle("pg_list_connections", h.listConnections))
	s.AddTool(queryTool, h.handle("pg_query", h.query))
	s.AddTool(schemaTool, h.handle("pg_schema", h.schema))
	s.AddTool(searchTool, h.handle("pg_search", h.search))
	s.AddTool(diffSchemaTool, h.handle("pg_diff_schema", h.diffSchema))
	s.AddTool(profileTool, h.handle("pg_profile", h.profile))
	s.AddTool(explainTool, h.handle("pg_explain", h.explain))
	s.AddTool(healthTool, h.handle("pg_health", h.health))
	return s
}

// toolFunc runs one call on its raw arguments, recording in c what the audit
// line reports, and returns the text of the result.
type toolFunc func(ctx context.Context, args json.RawMessage, c *call) (string, error)

// handle adapts run to the SDK: it logs the call, unless the call logged
// itself, and turns an error into a result with IsError set. The result
// stays within the inline limit with the notice it starts with: a tool fits
// its output in room, and handle cuts an error that does not fit there.
func (h *Handlers) handle(tool string, run toolFunc) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c := &call{tool: tool, start: time.Now()}
		text, err := run(ctx, req.Params.Arguments, c)
		if !c.audited {
			h.audit(c, err)
		}
		res := &mcp.CallToolResult{}
		if err != nil {
			text, res.IsError = cutMiddle(errorText(err), h.room(c)), true
		}
		res.Content = []mcp.Content{&mcp.TextContent{Text: c.notice + text}}
		return res, nil
	}
}

// room returns how many bytes the result of c may take after its notice,
// which room first cuts to half the inline limit: the role check's findings
// come first, one report per connection of a fan-out, and must leave the
// result room enough for at least a spill file's summary.
func (h *Handlers) room(c *call) int {
	c.notice = fitNotice(c.notice, h.spill.InlineMaxBytes/2)
	return h.spill.InlineMaxBytes - len(c.notice)
}

// fitNotice returns notice, the role check's findings that start a result,
// cut to limit bytes when longer: the lines that fit, then one that points to
// pg_list_connections, which shows every finding.
func fitNotice(notice string, limit int) string {
	if len(notice) <= limit {
		return notice
	}
	const more = "… more role check findings: pg_list_connections shows them all\n\n"
	head := notice[:max(limit-len(more), 0)]
	return head[:strings.LastIndexByte(head, '\n')+1] + more
}

// cutMiddle returns s when it fits in limit bytes. Otherwise it keeps the
// start and the end of s around a note of the bytes it drops, within limit:
// a Postgres error that quotes a long value keeps its SQLSTATE, position,
// detail and hint.
func cutMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// The note is never longer than it would be for all of s.
	keep := max(limit-len(cutNote(len(s))), 0)
	end := keep / 2
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	start := len(s) - (keep - keep/2)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:end] + cutNote(start-end) + s[start:]
}

// cutNote stands for the n bytes cutMiddle drops.
func cutNote(n int) string {
	return "…[" + strconv.Itoa(n) + " bytes cut]…"
}

// decode reads the arguments of a call into dst. Unknown arguments are an
// error, so a misspelled one fails loudly instead of being ignored. Numbers
// in untyped arguments keep their exact text (json.Number).
func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// useConnection checks that name is a configured connection and records it
// for the audit line.
func (h *Handlers) useConnection(name string, c *call) error {
	if _, ok := h.cfg.Connections[name]; !ok {
		return engine.UnknownConnection(name, h.cfg.Connections)
	}
	c.connection = name
	return nil
}

// checkName refuses name, given for the argument arg, when it is longer than
// Postgres keeps a name: Postgres would cut it to its first 63 bytes, which
// may name another object, and the tool would repeat it whole.
func checkName(arg, name string) error {
	if len(name) > config.MaxNameBytes {
		return fmt.Errorf("%s is %d bytes: Postgres names take at most %d, and it cuts a longer one", arg, len(name), config.MaxNameBytes)
	}
	return nil
}

// linesOutput writes lines, text a tool builds that is too large to return
// as it is, to a spill file as a result of one text column named column, one
// row per line, and returns its summary within limit bytes. It always writes
// the file: cut to 200 characters, the lines might fit inline, but without
// their ends.
func (h *Handlers) linesOutput(tool, conn, column string, lines []string, limit int) (string, error) {
	res := result.Text(column)
	for _, line := range lines {
		res.AddRow(line)
	}
	return h.spill.Spill(tool, conn, res, render.TSV, limit)
}
