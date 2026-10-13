package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/resolver"
)

// listConnections runs pg_list_connections: every connection, by name, with
// its role check once that has run, inline or, past the inline limit,
// spilled to a file, one line per row.
func (h *Handlers) listConnections(_ context.Context, raw json.RawMessage, c *call) (string, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return "", err
	}
	names := slices.Sorted(maps.Keys(h.cfg.Connections))
	lines := []string{fmt.Sprintf("Connections (%d):", len(names))}
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("  %s [%s]", name, strings.Join(flags(h.cfg.Connections[name]), ", ")))
		for _, line := range strings.Split(h.checks.status(name), "\n") {
			lines = append(lines, "    "+line)
		}
	}
	room := h.room(c)
	if text := strings.Join(lines, "\n"); len(text) <= room {
		return text, nil
	}
	return h.linesOutput("pg_list_connections", "", "line", lines, "", room)
}

// flags describes how a connection is configured.
func flags(conn config.Connection) []string {
	f := []string{conn.Source}
	if conn.RequiresVPN {
		f = append(f, "vpn")
	}
	if conn.DefaultSchema != "" {
		f = append(f, "schema:"+conn.DefaultSchema)
	}
	if status := resolver.StatusFor(conn); status != "" {
		f = append(f, status)
	}
	return f
}
