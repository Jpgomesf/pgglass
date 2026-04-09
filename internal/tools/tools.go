package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/output"
	"github.com/jpgomesf/mcp-postgres/internal/pool"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
)

// Handlers holds the dependencies needed by tool handlers.
type Handlers struct {
	Config config.Config
	Pool   *pool.ConnPool
}

// --- Input structs ---

type pgQueryInput struct {
	Connection string `json:"connection"`
	SQL        string `json:"sql"`
	Schema     string `json:"schema,omitempty"`
}

type pgSchemaInput struct {
	Connection string `json:"connection"`
	Schema     string `json:"schema,omitempty"`
	Table      string `json:"table,omitempty"`
}

// --- Helpers ---

func parseArgs(req *mcp.CallToolRequest, dst any) error {
	data, err := json.Marshal(req.Params.Arguments)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func txt(text string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil
}

func errTxt(format string, args ...any) (*mcp.CallToolResult, error) {
	return txt(fmt.Sprintf("Error: "+format, args...))
}

// --- Tool handlers ---

// HandleListConnections returns all configured connections and their status.
func (h *Handlers) HandleListConnections(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var lines []string
	for name, conn := range h.Config.Connections {
		flags := []string{conn.Source}
		if conn.IsReadonly(h.Config.Defaults) {
			flags = append(flags, "readonly")
		}
		if conn.RequiresVPN {
			flags = append(flags, "vpn")
		}
		if conn.DefaultSchema != "" {
			flags = append(flags, "schema:"+conn.DefaultSchema)
		}
		if status := resolver.StatusFor(conn); status != "" {
			flags = append(flags, status)
		}
		lines = append(lines, fmt.Sprintf("  %s [%s]", name, strings.Join(flags, ", ")))
	}

	header := fmt.Sprintf("Connections (%d):", len(h.Config.Connections))
	return txt(header + "\n" + strings.Join(lines, "\n"))
}

// HandleQuery executes a read-only SQL query against a named connection.
func (h *Handlers) HandleQuery(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var input pgQueryInput
	if err := parseArgs(req, &input); err != nil {
		return errTxt("invalid arguments: %v", err)
	}

	sql := strings.TrimSpace(input.SQL)
	if sql == "" {
		return errTxt("empty SQL statement.")
	}

	firstWord := strings.ToUpper(strings.Fields(sql)[0])
	if config.BlockedSQL[firstWord] {
		return errTxt("'%s' statements are not allowed.", firstWord)
	}

	schema := input.Schema
	if schema == "" {
		if connCfg, ok := h.Config.Connections[input.Connection]; ok {
			schema = connCfg.DefaultSchema
		}
	}

	conn, err := h.Pool.OpenConnection(ctx, h.Config, input.Connection)
	if err != nil {
		return errTxt("%v", err)
	}
	defer pool.ReturnConnection(ctx, conn)

	if schema != "" {
		_, err := conn.Exec(ctx, fmt.Sprintf(
			"SET search_path TO %s", pgx.Identifier{schema}.Sanitize()))
		if err != nil {
			return errTxt("setting search_path: %v", err)
		}
	}

	headers, records, err := execQuery(ctx, conn, sql)
	if err != nil {
		return errTxt("database: %v", err)
	}

	if len(records) == 0 {
		return txt("Query returned no rows.")
	}

	prefix := ""
	if schema != "" {
		prefix = fmt.Sprintf("[search_path: %s]\n", schema)
	}

	table := output.FormatTable(records, headers)
	rowLabel := "rows"
	if len(records) == 1 {
		rowLabel = "row"
	}
	out := fmt.Sprintf("%s%s\n(%d %s)", prefix, table, len(records), rowLabel)

	return txt(output.SmartOutput(out, "pg_query", len(records), headers))
}

// HandleSchema lists tables and columns for a named connection.
func (h *Handlers) HandleSchema(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var input pgSchemaInput
	if err := parseArgs(req, &input); err != nil {
		return errTxt("invalid arguments: %v", err)
	}

	schemaFilter := input.Schema
	if schemaFilter == "" {
		if connCfg, ok := h.Config.Connections[input.Connection]; ok {
			schemaFilter = connCfg.DefaultSchema
		}
	}

	conn, err := h.Pool.OpenConnection(ctx, h.Config, input.Connection)
	if err != nil {
		return errTxt("%v", err)
	}
	defer pool.ReturnConnection(ctx, conn)

	var targetSchemas []string
	if schemaFilter != "" {
		targetSchemas = []string{schemaFilter}
	} else {
		targetSchemas, err = discoverSchemas(ctx, conn)
		if err != nil {
			return errTxt("%v", err)
		}
		if len(targetSchemas) == 0 {
			return txt("No user schemas found.")
		}
	}

	columns, err := fetchColumns(ctx, conn, targetSchemas, input.Table)
	if err != nil {
		return errTxt("%v", err)
	}

	if len(columns) == 0 {
		return txt(noTablesMessage(schemaFilter, input.Table))
	}

	out := formatSchemaTree(columns)
	return txt(output.SmartOutput(out, "pg_schema", len(columns),
		[]string{"table_schema", "table_name", "column_name"}))
}

// --- Schema/query helpers ---

func execQuery(ctx context.Context, conn *pgx.Conn, sql string) ([]string, []map[string]string, error) {
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	headers := fieldNames(rows)
	var records []map[string]string

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		record := make(map[string]string, len(headers))
		for i, h := range headers {
			record[h] = fmt.Sprintf("%v", values[i])
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return headers, records, nil
}

func discoverSchemas(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx,
		"SELECT schema_name FROM information_schema.schemata ORDER BY schema_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !config.SystemSchemas[name] && !strings.HasPrefix(name, "pg_") {
			schemas = append(schemas, name)
		}
	}
	return schemas, rows.Err()
}

type columnInfo struct {
	Schema   string
	Table    string
	Column   string
	DataType string
	Nullable string
	Default  *string
}

func fetchColumns(ctx context.Context, conn *pgx.Conn, schemas []string, tableFilter string) ([]columnInfo, error) {
	query := `
		SELECT table_schema, table_name, column_name,
		       data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = ANY($1)`
	args := []any{schemas}

	if tableFilter != "" {
		query += " AND table_name = $2"
		args = append(args, tableFilter)
	}
	query += " ORDER BY table_schema, table_name, ordinal_position"

	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.Schema, &c.Table, &c.Column, &c.DataType, &c.Nullable, &c.Default); err != nil {
			return nil, err
		}
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

func formatSchemaTree(columns []columnInfo) string {
	type tableEntry struct {
		name    string
		columns []columnInfo
	}

	schemaMap := make(map[string]map[string]*tableEntry)
	var schemaOrder []string

	for _, c := range columns {
		if _, ok := schemaMap[c.Schema]; !ok {
			schemaMap[c.Schema] = make(map[string]*tableEntry)
			schemaOrder = append(schemaOrder, c.Schema)
		}
		tables := schemaMap[c.Schema]
		if _, ok := tables[c.Table]; !ok {
			tables[c.Table] = &tableEntry{name: c.Table}
		}
		tables[c.Table].columns = append(tables[c.Table].columns, c)
	}

	var sb strings.Builder
	for _, sName := range schemaOrder {
		fmt.Fprintf(&sb, "Schema: %s\n", sName)
		for _, te := range schemaMap[sName] {
			fmt.Fprintf(&sb, "  Table: %s\n", te.name)
			for _, col := range te.columns {
				nullable := ""
				if col.Nullable != "YES" {
					nullable = " NOT NULL"
				}
				def := ""
				if col.Default != nil {
					def = fmt.Sprintf(" DEFAULT %s", *col.Default)
				}
				fmt.Fprintf(&sb, "    %s: %s%s%s\n",
					col.Column, col.DataType, nullable, def)
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func noTablesMessage(schemaFilter, tableFilter string) string {
	msg := "No tables found"
	if schemaFilter != "" {
		msg += fmt.Sprintf(" in schema '%s'", schemaFilter)
	}
	if tableFilter != "" {
		msg += fmt.Sprintf(" matching table '%s'", tableFilter)
	}
	return msg + "."
}

func fieldNames(rows pgx.Rows) []string {
	descs := rows.FieldDescriptions()
	names := make([]string, len(descs))
	for i, d := range descs {
		names[i] = d.Name
	}
	return names
}
