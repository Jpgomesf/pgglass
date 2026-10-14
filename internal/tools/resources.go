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

func (h *Handlers) readTable(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	c := &call{tool: "resource", start: time.Now()}
	text, err := h.describeTable(ctx, uri, c)
	h.audit(c, err)
	if err != nil {
		return nil, h.resourceError(uri, err, c)
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
		var wire *jsonrpc.Error
		if errors.As(notFound, &wire) {
			wire.Message = c.notice + wire.Message
		}
		return notFound
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: c.notice + cutMiddle(errorText(err), room)}
}

func (h *Handlers) withTableResources(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if list, ok := res.(*mcp.ListResourcesResult); ok && err == nil {
			list.Resources = append(list.Resources, h.listTableResources(ctx)...)
		}
		return res, err
	}
}

func (h *Handlers) withinInlineLimit(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		var wire *jsonrpc.Error
		if !errors.As(err, &wire) {
			return res, err
		}
		if read, ok := req.(*mcp.ReadResourceRequest); ok && len(wire.Data) > 0 {
			wire.Data = uriData(read.Params.URI, h.spill.InlineMaxBytes-len(wire.Message))
		}
		if room := h.spill.InlineMaxBytes - len(wire.Data); len(wire.Message) > room {
			wire.Message = cutMiddle(wire.Message, room)
		}
		return res, err
	}
}

const escapedByteMax = len(`\u0000`)

func uriData(uri string, limit int) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"uri": uri})
	if len(data) > limit {
		data, _ = json.Marshal(map[string]string{"uri": cutMiddle(uri, (limit-len(`{"uri":""}`))/escapedByteMax)})
	}
	return data
}

func (h *Handlers) listTableResources(ctx context.Context) []*mcp.Resource {
	conns := h.checks.inUse()
	lists := make([][]*mcp.Resource, len(conns))
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Go(func() {
			c := &call{tool: "resource_list", start: time.Now(), connection: conn}
			resources, err := h.connectionResources(ctx, conn)
			c.rows = len(resources)
			h.audit(c, err)
			lists[i] = resources
		})
	}
	wg.Wait()
	return slices.Concat(lists...)
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
