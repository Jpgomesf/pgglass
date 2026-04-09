package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := LoadConfig(); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-postgres",
			Version: "1.0.0",
		},
		nil,
	)

	server.AddTool(&mcp.Tool{
		Name: "pg_list_connections",
		Description: "List all configured database connections and their status. " +
			"Use pg_schema to discover live schemas for a specific connection.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, HandleListConnections)

	server.AddTool(&mcp.Tool{
		Name: "pg_query",
		Description: "Execute a read-only SQL query against a named database connection. " +
			"Always runs inside a READ ONLY transaction. " +
			"Use pg_schema to inspect available tables first. " +
			"Pass 'schema' to set the search_path before executing. " +
			"Multiple pg_query calls can run in parallel against different connections for faster results.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{"type": "string", "description": "Connection name from config.json."},
				"sql":        map[string]any{"type": "string", "description": "SQL SELECT statement to execute. Must be read-only."},
				"schema":     map[string]any{"type": "string", "description": "Optional schema to set as search_path."},
			},
			"required": []string{"connection", "sql"},
		},
	}, HandleQuery)

	server.AddTool(&mcp.Tool{
		Name: "pg_schema",
		Description: "List tables and their columns for a named database connection. " +
			"Auto-discovers all available schemas unless 'schema' is specified. " +
			"Use this before pg_query to understand what is available. " +
			"Can be called in parallel across multiple connections.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{"type": "string", "description": "Connection name from config.json."},
				"schema":     map[string]any{"type": "string", "description": "Optional schema filter."},
				"table":      map[string]any{"type": "string", "description": "Optional table name filter."},
			},
			"required": []string{"connection"},
		},
	}, HandleSchema)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
