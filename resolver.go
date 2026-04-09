package main

import (
	"context"
	"fmt"
	"strings"
)

// CredentialSource resolves a DSN from a connection configuration.
// Each implementation handles one source type (direct, env, dotenv, aws_secrets_manager).
type CredentialSource interface {
	// Type returns the source identifier used in config.json (e.g. "direct", "env").
	Type() string

	// Resolve returns a libpq-compatible DSN string for the given connection.
	Resolve(ctx context.Context, connName string, conn Connection) (string, error)
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

// ResolveCredentials dispatches to the appropriate CredentialSource based on conn.Source.
func ResolveCredentials(ctx context.Context, connName string, conn Connection) (string, error) {
	src, ok := sources[conn.Source]
	if !ok {
		return "", fmt.Errorf("connection %q: unknown source %q (registered: %s)",
			connName, conn.Source, strings.Join(RegisteredSourceTypes(), ", "))
	}
	return src.Resolve(ctx, connName, conn)
}
