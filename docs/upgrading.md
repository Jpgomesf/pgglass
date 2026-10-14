# Upgrading from mcp-postgres

pgglass was called mcp-postgres. The tools are the same, and it reads the same `config.json`.

- mcp-postgres read the `config.json` next to its binary, else the one in its working directory; pgglass reads neither. Move that file, and any `.env` file its connections read, to `~/.config/pgglass/` (or `$XDG_CONFIG_HOME/pgglass/`), or pass the file's path with `--config`.
- A relative `dotenv_path`, and the default `.env`, still resolve against the server's working directory, which the MCP client picks, not against the directory of `config.json`: set `dotenv_path` to the moved `.env`'s absolute path.
- [Install](../README.md#install) pgglass, then rename the server in your MCP client and point it at the new binary. In Claude Code, run `claude mcp remove mcp-postgres` and [add pgglass](../README.md#add-to-claude-code), or rename the `mcp-postgres` entry under `mcpServers` in `~/.claude.json` or `.mcp.json` to `pgglass` and update its `command`.
- Rename the permission rules in your Claude Code settings that name the server: `mcp__mcp-postgres` becomes `mcp__pgglass`, and `mcp__mcp-postgres__pg_query` becomes `mcp__pgglass__pg_query`.
- Spill files now go to `pgglass` under the user cache directory, so the old `mcp-postgres` directory there can be deleted.
