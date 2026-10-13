package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/jpgomesf/pgglass/internal/config"
)

// CredentialSource resolves a DSN from a connection configuration.
// Each implementation handles one source type (direct, env, dotenv, aws_secrets_manager).
type CredentialSource interface {
	// Type returns the source identifier used in config.json (e.g. "direct", "env").
	Type() string

	// Resolve returns a libpq-compatible DSN string for the given connection.
	// The engine calls it when the connection is first used and again for each
	// new database connection, so a source should cache what is slow to fetch,
	// and implement Invalidator if it does.
	Resolve(ctx context.Context, connName string, conn config.Connection) (string, error)

	// Status returns a short status hint for display in pg_list_connections.
	// Return "" if the source has no meaningful status to report.
	Status(conn config.Connection) string
}

// Invalidator is implemented by a CredentialSource that caches credentials.
type Invalidator interface {
	// Invalidate drops what the source cached for conn, so that its next
	// Resolve fetches the credentials afresh. The engine calls it when the
	// server refuses the credentials of a new database connection.
	Invalidate(conn config.Connection)
}

// sources maps source type strings to their implementations.
var sources = map[string]CredentialSource{}

// RegisterSource registers a CredentialSource implementation.
// Call this in init() functions to add custom sources.
func RegisterSource(s CredentialSource) {
	sources[s.Type()] = s
}

// RegisteredSourceTypes returns the list of registered source type names.
func RegisteredSourceTypes() []string {
	types := make([]string, 0, len(sources))
	for t := range sources {
		types = append(types, t)
	}
	return types
}

// IsRegistered returns true if the given source type has been registered.
func IsRegistered(sourceType string) bool {
	_, ok := sources[sourceType]
	return ok
}

// StatusFor returns the status hint for a connection from its registered source.
func StatusFor(conn config.Connection) string {
	src, ok := sources[conn.Source]
	if !ok {
		return ""
	}
	return src.Status(conn)
}

// Resolve dispatches to the appropriate CredentialSource based on conn.Source.
func Resolve(ctx context.Context, connName string, conn config.Connection) (string, error) {
	src, ok := sources[conn.Source]
	if !ok {
		return "", fmt.Errorf("connection %q: unknown source %q (registered: %s)",
			connName, conn.Source, strings.Join(RegisteredSourceTypes(), ", "))
	}
	return src.Resolve(ctx, connName, conn)
}

// Invalidate makes the source of conn drop what it cached for conn. It
// reports whether the source caches credentials: only then can resolving
// conn again return others.
func Invalidate(conn config.Connection) bool {
	inv, ok := sources[conn.Source].(Invalidator)
	if ok {
		inv.Invalidate(conn)
	}
	return ok
}
