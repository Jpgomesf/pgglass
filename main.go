// Command mcp-postgres is a stdio MCP server that gives AI agents read-only
// access to the PostgreSQL connections in config.json.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
	"github.com/jpgomesf/mcp-postgres/internal/tools"
)

// version is set at release time (goreleaser's -X main.version).
var version = "dev"

func main() {
	// stdout carries the protocol: every log line goes to stderr.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("mcp-postgres stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run loads config.json and serves the tools over stdin and stdout until the
// client disconnects.
func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := validateSources(cfg); err != nil {
		return err
	}
	if cfg.UsesReadonly() {
		logger.Warn("config: readonly is deprecated and ignored: the server is always read-only")
	}

	// A per-user directory: in a shared temp directory, another user could
	// create it first and replace the files the agent is told to read.
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("spill directory: %w", err)
	}
	store := &spill.Store{
		Dir:            filepath.Join(cacheDir, "mcp-postgres"),
		TTL:            time.Duration(cfg.Defaults.SpillTTLHours) * time.Hour,
		InlineMaxBytes: config.InlineMaxBytes,
	}
	if err := store.Cleanup(); err != nil {
		logger.Warn("spill cleanup failed", slog.String("error", err.Error()))
	}

	eng := engine.New(&cfg)
	defer eng.Close()
	server := tools.NewServer(tools.New(cfg, eng, store, os.Stderr), version)
	return server.Run(ctx, &mcp.StdioTransport{})
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
