// Command mcp-postgres is a stdio MCP server that gives AI agents read-only
// access to the PostgreSQL connections in config.json.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

	dir, err := spillDir()
	if err != nil {
		return fmt.Errorf("spill directory: %w", err)
	}
	store := &spill.Store{
		Dir:            dir,
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

// spillDir returns the directory for spill files: mcp-postgres under the
// user cache directory or, without one (HOME unset), mcp-postgres-<euid> in
// the temp directory, which it creates. In a shared temp directory, another
// user could create that one first and read or replace the files the agent
// is told to read, so it must be a directory, not a symlink, that only this
// user may use.
func spillDir() (string, error) {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "mcp-postgres"), nil
	}
	dir := filepath.Join(os.TempDir(), "mcp-postgres-"+strconv.Itoa(os.Geteuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !private(info, os.Geteuid()) {
		return "", fmt.Errorf("%s must be a directory of this user's with mode 0700", dir)
	}
	return dir, nil
}

// private reports whether info describes a directory, not a symlink, that
// uid owns and that no one else may use: mode 0700.
func private(info fs.FileInfo, uid int) bool {
	owner, ok := fileOwner(info)
	return ok && owner == uid && info.IsDir() && info.Mode().Perm() == 0o700
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
