package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
)

// listConnections runs pg_list_connections: every connection, by name, with
// its role check once that has run.
func (h *Handlers) listConnections(_ context.Context, raw json.RawMessage, _ *call) (string, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return "", err
	}
	names := slices.Sorted(maps.Keys(h.cfg.Connections))
	var b strings.Builder
	fmt.Fprintf(&b, "Connections (%d):", len(names))
	for _, name := range names {
		fmt.Fprintf(&b, "\n  %s [%s]", name, strings.Join(flags(h.cfg.Connections[name]), ", "))
		for _, line := range strings.Split(h.checks.status(name), "\n") {
			b.WriteString("\n    " + line)
		}
	}
	return b.String(), nil
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
