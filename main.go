package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/pool"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
	"github.com/jpgomesf/mcp-postgres/internal/tools"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if err := validateSources(cfg); err != nil {
		log.Fatalf("config validation: %v", err)
	}

	connPool := pool.New(config.DefaultConnectionTTL)
	h := &tools.Handlers{Config: cfg, Pool: connPool}

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
	}, h.HandleListConnections)

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
	}, h.HandleQuery)

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
	}, h.HandleSchema)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func validateSources(cfg config.Config) error {
	for name, conn := range cfg.Connections {
		if !resolver.IsRegistered(conn.Source) {
			return fmt.Errorf("connection %q: unknown source %q (registered: %s)",
				name, conn.Source, strings.Join(resolver.RegisteredSourceTypes(), ", "))
		}
	}
	return nil
}
