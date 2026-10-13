package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

// annotations hold for every tool: none changes anything.
var annotations = &mcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: ptr(false),
	IdempotentHint:  true,
	OpenWorldHint:   ptr(false),
}

var connectionArg = prop("string", "Connection name, from pg_list_connections.")

var listConnectionsTool = &mcp.Tool{
	Name:        "pg_list_connections",
	Description: "List the configured connections and, once checked, what each connection's role can do beyond reading.",
	Annotations: annotations,
	InputSchema: object(nil, map[string]any{}),
}

var queryTool = &mcp.Tool{
	Name: "pg_query",
	Description: "Run one SELECT, EXPLAIN SELECT or SHOW in a READ ONLY transaction that is always rolled back. " +
		"Other statements and functions with side effects are rejected. " +
		"Rows past max_rows are cut; a result too large to return is saved to a CSV file and summarized.",
	Annotations: annotations,
	InputSchema: object([]string{"sql"}, map[string]any{
		"connection": connectionArg,
		"sql":        prop("string", "One statement; $1, $2, ... take params."),
		"params":     prop("array", "Values for $1, $2, ..."),
		"schema":     prop("string", "search_path for this query; default: the connection's default_schema."),
		"format": map[string]any{
			"type":        "string",
			"enum":        config.Formats,
			"description": "Result format; default from config.",
		},
		"max_rows": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "Row cap for this query; it can only lower the configured cap.",
		},
		"connections": fanoutConnectionsArg,
	}),
}

var schemaTool = &mcp.Tool{
	Name: "pg_schema",
	Description: "Describe a database step by step: without schema, list the schemas; with schema, its tables and views; " +
		"with schema and table, that relation's columns, constraints, indexes, policies and triggers.",
	Annotations: annotations,
	InputSchema: object([]string{"connection"}, map[string]any{
		"connection": connectionArg,
		"schema":     prop("string", "Schema to list."),
		"table":      prop("string", "Relation to describe; its schema defaults to the connection's default_schema."),
	}),
}

var searchTool = &mcp.Tool{
	Name:        "pg_search",
	Description: "Find relations, columns and functions whose name contains pattern, ignoring case; pattern has no wildcards.",
	Annotations: annotations,
	InputSchema: object([]string{"connection", "pattern"}, map[string]any{
		"connection": connectionArg,
		"pattern":    prop("string", "Text the name contains."),
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "Most matches to return; default 50.",
		},
	}),
}

func object(required []string, props map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func ptr[T any](v T) *T { return &v }

// fanoutConnectionsArg lets pg_query fan out: see fanout.
var fanoutConnectionsArg = map[string]any{
	"type":        "array",
	"items":       map[string]any{"type": "string"},
	"maxItems":    maxFanout,
	"description": "Instead of connection: run on each, in parallel; rows gain a first column, connection.",
}

var diffSchemaTool = &mcp.Tool{
	Name: "pg_diff_schema",
	Description: "Compare one schema on two connections, e.g. prod and staging: relations, columns, " +
		"constraints, indexes and RLS policies.",
	Annotations: annotations,
	InputSchema: object([]string{"left", "right"}, map[string]any{
		"left":   connectionArg,
		"right":  connectionArg,
		"schema": prop("string", "Schema to compare; default public."),
	}),
}

var profileTool = &mcp.Tool{
	Name: "pg_profile",
	Description: "Profile a table's columns from pg_stats: nulls, distinct values, top 5 values, min/max; " +
		"if never analyzed, counts in a sample of up to 10000 rows.",
	Annotations: annotations,
	InputSchema: object([]string{"connection", "table"}, map[string]any{
		"connection": connectionArg,
		"table":      prop("string", "schema.table, or a table in schema."),
		"schema":     prop("string", "Schema of table; default: default_schema."),
		"column":     prop("string", "Only this column."),
	}),
}

var explainTool = &mcp.Tool{
	Name:        "pg_explain",
	Description: "Summarize one SELECT's plan: cost, costliest nodes and flags (big seq scans, misestimates, disk spills, big nested loops).",
	Annotations: annotations,
	InputSchema: object([]string{"connection", "sql"}, map[string]any{
		"connection": connectionArg,
		"sql":        prop("string", "One SELECT."),
		"analyze":    prop("boolean", "Run it, rolled back, for actual times and rows; default false."),
		"schema":     prop("string", "search_path; default: the connection's default_schema."),
	}),
}
