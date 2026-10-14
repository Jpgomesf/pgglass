// Package tools implements the MCP tools: pg_list_connections, pg_schema,
// pg_search, pg_query, pg_explain, pg_profile, pg_diff_schema and pg_health,
// and the table resources, pgglass://<connection>/<schema>/<table>.
//
// Every tool failure is a tool result with IsError set, never a protocol
// error; a resource read fails with a JSON-RPC error. Every call writes one
// JSON audit line, or one per connection when pg_query fans out or the
// resources are listed, that holds no SQL text, parameter or value.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/spill"
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
	tables  *introspect.Cache
}

// New returns the Handlers for cfg, whose defaults must be filled in. They
// query through eng, spill to store and write JSON audit lines to auditLog.
func New(cfg config.Config, eng *engine.Manager, store *spill.Store, auditLog io.Writer) *Handlers {
	return &Handlers{
		cfg:     cfg,
		engine:  eng,
		spill:   store,
		schemas: introspect.NewCache(introspect.DefaultCacheTTL),
		checks:  newRoleChecks(cfg.Defaults.RoleCheck, eng),
		log:     slog.New(slog.NewJSONHandler(auditLog, nil)),
		tables:  introspect.NewCache(introspect.DefaultCacheTTL),
	}
}

// NewServer returns an MCP server, named pgglass, that exposes the tools
// and table resources of h and tells clients how to use the tools well.
// It lists the tables of the connections that calls have used, and tells
// clients when a call first uses one. Served over HTTP, it keeps no
// sessions, so only a client on revision 2026-07-28 or later can confirm a
// statement.
func NewServer(h *Handlers, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:        "pgglass",
		Title:       "pgglass",
		Description: "Read-only PostgreSQL for AI agents across many databases, enforced by the server",
		Version:     version,
		WebsiteURL:  "https://github.com/Jpgomesf/pgglass",
	}, &mcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}, Resources: &mcp.ResourceCapabilities{ListChanged: true}},
	})
	s.AddTool(listConnectionsTool, h.handle("pg_list_connections", h.listConnections))
	s.AddTool(queryTool, h.handle("pg_query", h.query))
	s.AddTool(schemaTool, h.handle("pg_schema", h.schema))
	s.AddTool(searchTool, h.handle("pg_search", h.search))
	s.AddTool(diffSchemaTool, h.handle("pg_diff_schema", h.diffSchema))
	s.AddTool(profileTool, h.handle("pg_profile", h.profile))
	s.AddTool(explainTool, h.handle("pg_explain", h.explain))
	s.AddTool(healthTool, h.handle("pg_health", h.health))
	s.AddResourceTemplate(tableTemplate, h.readTable)
	s.AddReceivingMiddleware(h.withTableResources, h.withinInlineLimit)
	h.checks.onFirstUse = func() { s.AddResourceTemplate(tableTemplate, h.readTable) }
	return s
}

type toolFunc func(ctx context.Context, args json.RawMessage, c *call) (string, error)

func (h *Handlers) handle(tool string, run toolFunc) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c := &call{tool: tool, start: time.Now(), answers: req.Params.InputResponses, canAsk: canAsk(req)}
		text, err := run(ctx, req.Params.Arguments, c)
		var ask *confirmRequest
		if errors.As(err, &ask) {
			return ask.result(), nil
		}
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

func (h *Handlers) room(c *call) int {
	c.notice = fitNotice(c.notice, h.spill.InlineMaxBytes/2)
	return h.spill.InlineMaxBytes - len(c.notice)
}

func fitNotice(notice string, limit int) string {
	if len(notice) <= limit {
		return notice
	}
	const more = "… more role check findings: pg_list_connections shows them all\n\n"
	head := notice[:max(limit-len(more), 0)]
	return head[:strings.LastIndexByte(head, '\n')+1] + more
}

func cutMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
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

func cutNote(n int) string {
	return "…[" + strconv.Itoa(n) + " bytes cut]…"
}

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

func (h *Handlers) useConnection(name string, c *call) error {
	if _, ok := h.cfg.Connections[name]; !ok {
		return engine.UnknownConnection(name, h.cfg.Connections)
	}
	c.connection = name
	return nil
}

func checkName(arg, name string) error {
	if len(name) > config.MaxNameBytes {
		return fmt.Errorf("%s is %d bytes: Postgres names take at most %d, and it cuts a longer one", arg, len(name), config.MaxNameBytes)
	}
	return nil
}

func (h *Handlers) linesOutput(tool, conn, column string, lines []string, cut string, limit int) (string, error) {
	res := result.Text(column)
	for _, line := range lines {
		res.AddRow(line)
	}
	res.Truncated, res.TruncatedReason = cut != "", cut
	return h.spill.OutputWhole(tool, conn, res, render.TSV, limit)
}
