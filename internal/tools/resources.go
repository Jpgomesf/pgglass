package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/spill"
)

const (
	tableScheme            = "pgglass://"
	tableMIMEType          = "text/plain"
	resourcesPerConnection = 200
)

var tableTemplate = &mcp.ResourceTemplate{
	URITemplate: tableScheme + "{connection}/{schema}/{table}",
	Name:        "table",
	Title:       "Table or view",
	Description: "A table, view or other relation as pg_schema describes it: columns, constraints, indexes, policies and triggers.",
	MIMEType:    tableMIMEType,
}

var errNoResource = errors.New("no table resource has this URI")

type tableURI struct {
	conn, schema, table string
}

func (u tableURI) String() string {
	return tableScheme + escapeSegment(u.conn) + "/" + escapeSegment(u.schema) + "/" + escapeSegment(u.table)
}

func escapeSegment(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func parseTableURI(uri string) (tableURI, bool) {
	rest, ok := strings.CutPrefix(uri, tableScheme)
	segments := strings.Split(rest, "/")
	if !ok || len(segments) != 3 {
		return tableURI{}, false
	}
	for i, segment := range segments {
		name, err := url.PathUnescape(segment)
		if err != nil {
			return tableURI{}, false
		}
		segments[i] = name
	}
	u := tableURI{conn: segments[0], schema: segments[1], table: segments[2]}
	if !postgresName(u.schema) || !postgresName(u.table) {
		return tableURI{}, false
	}
	return u, true
}

func postgresName(s string) bool {
	return s != "" && len(s) <= config.MaxNameBytes && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

type readCall struct{}

func (h *Handlers) auditReads(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		read, ok := req.(*mcp.ReadResourceRequest)
		if !ok {
			return next(ctx, method, req)
		}
		c := &call{tool: "resource", start: time.Now()}
		res, err := next(context.WithValue(ctx, readCall{}, c), method, req)
		h.audit(c, err)
		var wire *jsonrpc.Error
		if err != nil && !errors.As(err, &wire) {
			err = h.resourceError(read.Params.URI, err, c)
		}
		return res, err
	}
}

func (h *Handlers) readTable(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	c := ctx.Value(readCall{}).(*call)
	text, err := h.describeTable(ctx, uri, c)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: tableMIMEType, Text: c.notice + text}}}, nil
}

func (h *Handlers) describeTable(ctx context.Context, uri string, c *call) (string, error) {
	u, ok := parseTableURI(uri)
	_, configured := h.cfg.Connections[u.conn]
	if !ok || !configured {
		return "", errNoResource
	}
	args, err := json.Marshal(schemaArgs{Connection: u.conn, Schema: u.schema, Table: u.table})
	if err != nil {
		return "", err
	}
	return h.schema(ctx, args, c)
}

func (h *Handlers) resourceError(uri string, err error, c *call) error {
	room := h.room(c)
	if errors.Is(err, errNoResource) || errors.Is(err, introspect.ErrNotFound) {
		notFound := mcp.ResourceNotFoundError(uri)
		if wire, ok := errors.AsType[*jsonrpc.Error](notFound); ok {
			wire.Message = c.notice + wire.Message
		}
		return notFound
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: c.notice + spill.CutMiddle(errorText(err), room)}
}

func (h *Handlers) withTableResources(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if list, ok := res.(*mcp.ListResourcesResult); ok && err == nil {
			list.Resources = append(list.Resources, h.listTableResources(ctx, h.listRoom(*list))...)
		}
		return res, err
	}
}

func (h *Handlers) listRoom(sent mcp.ListResourcesResult) int {
	sent.Meta = mcp.Meta{mcp.MetaKeyServerInfo: h.serverInfo}
	sent.ResultType = "complete"
	data, _ := json.Marshal(&sent)
	return h.spill.InlineMaxBytes - len(data)
}

func (h *Handlers) withinInlineLimit(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		wire, ok := errors.AsType[*jsonrpc.Error](err)
		if !ok {
			return res, err
		}
		if wire != err {
			wire = &jsonrpc.Error{Code: wire.Code, Message: err.Error()}
		}
		if read, ok := req.(*mcp.ReadResourceRequest); ok && len(wire.Data) > 0 {
			wire.Data = uriData(read.Params.URI, h.spill.InlineMaxBytes-len(wire.Message))
		}
		if room := h.spill.InlineMaxBytes - len(wire.Data); len(wire.Message) > room {
			wire.Message = spill.CutMiddle(wire.Message, room)
		}
		return res, wire
	}
}

const escapedByteMax = len(`\u0000`)

func uriData(uri string, limit int) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"uri": uri})
	if len(data) > limit {
		data, _ = json.Marshal(map[string]string{"uri": spill.CutMiddle(uri, (limit-len(`{"uri":""}`))/escapedByteMax)})
	}
	return data
}

func (h *Handlers) listTableResources(ctx context.Context, room int) []*mcp.Resource {
	conns := h.checks.inUse()
	lists := make([][]*mcp.Resource, len(conns))
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Go(func() {
			c := &call{tool: "resource_list", start: time.Now(), connection: conn}
			resources, err := h.connectionResources(ctx, conn)
			lists[i] = firstWithin(resources, room/len(conns))
			c.rows = len(lists[i])
			h.audit(c, err)
		})
	}
	wg.Wait()
	return slices.Concat(lists...)
}

func firstWithin(resources []*mcp.Resource, room int) []*mcp.Resource {
	for i, r := range resources {
		data, _ := json.Marshal(r)
		if room -= len(data) + len(","); room < 0 {
			return resources[:i]
		}
	}
	return resources
}

func (h *Handlers) connectionResources(ctx context.Context, conn string) ([]*mcp.Resource, error) {
	if err := h.checks.admit(ctx, conn); err != nil {
		return nil, err
	}
	listed, err := h.tables.Get(conn, func() (string, error) {
		return firstRelationsJSON(ctx, h.engine.Querier(conn))
	})
	if err != nil {
		return nil, &dbError{err}
	}
	var relations introspect.RelationList
	if err := json.Unmarshal([]byte(listed), &relations); err != nil {
		return nil, err
	}
	resources := make([]*mcp.Resource, len(relations))
	for i, r := range relations {
		resources[i] = &mcp.Resource{
			URI:         tableURI{conn: conn, schema: r.Schema, table: r.Name}.String(),
			Name:        conn + ": " + r.Schema + "." + r.Name,
			Description: r.Kind,
			MIMEType:    tableMIMEType,
		}
	}
	return resources, nil
}

func firstRelationsJSON(ctx context.Context, q engine.Querier) (string, error) {
	schemas, _, err := introspect.FirstSchemas(ctx, q, listLimit)
	if err != nil {
		return "", err
	}
	relations := introspect.RelationList{}
	for _, s := range schemas {
		if len(relations) == resourcesPerConnection {
			break
		}
		if s.Relations == 0 {
			continue
		}
		first, _, err := introspect.FirstTables(ctx, q, s.Name, resourcesPerConnection-len(relations))
		if err != nil {
			return "", err
		}
		relations = append(relations, first...)
	}
	listed, err := json.Marshal(relations)
	return string(listed), err
}
