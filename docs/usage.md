# Usage Guide

Once the server is running and connected to Claude Code, you interact with your databases through natural language. The AI assistant calls the MCP tools on your behalf.

## Discover your connections

> "What databases do I have access to?"

Claude calls `pg_list_connections` and returns:

```
Connections (3):
  local-dev [direct, readonly]
  staging-app [env, readonly, schema:app]
  prod-app [aws_secrets_manager, readonly, vpn, schema:app, loaded]
```

## Explore the schema

> "Show me the tables in staging-app"

Claude calls `pg_schema` with `connection: "staging-app"`. You get back a tree of schemas, tables, and columns with types — enough context for Claude to write accurate queries.

You can also filter:

> "Show me just the users table in prod-app"

## Query

> "How many active users signed up this month in staging?"

Claude writes the SQL, calls `pg_query`, and returns formatted results. All queries are enforced read-only — Claude cannot modify your data even if it tries.

## Parallel Queries

This is where the server shines. Claude can fire multiple tool calls **in parallel** against different connections.

> "Compare the row count of the orders table between staging and production"

Two `pg_query` calls run simultaneously — one against `staging-app`, one against `prod-app`. No sequential waiting.

More examples:

> "Check if the users table schema matches between staging and prod"
>
> "Find all tables that have a column named 'tenant_id' across all my connections"
>
> "Show me the last 10 orders in prod and the matching records in the analytics db"

## Tips

- **Start with `pg_schema`** — Claude writes better queries when it knows the exact column names and types.
- **Use `default_schema`** in your config to avoid specifying the schema every time.
- **Large results** are automatically saved to a temp file with a summary. Claude can then read specific parts instead of flooding the conversation.
- **Readonly is your friend** — keep it enabled in production. The server blocks writes at two levels: SQL keyword filtering and PostgreSQL's `default_transaction_read_only` mode.
