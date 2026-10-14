package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/resolver"
	"github.com/jpgomesf/pgglass/internal/spill"
	"github.com/jpgomesf/pgglass/internal/tools"
)

var warmParser = guard.Warm

func serve(ctx context.Context, opts options, stdin io.Reader, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := serveTools(ctx, opts, stdin, stdout, stderr, logger); err != nil {
		logger.Error("pgglass stopped", slog.String("error", err.Error()))
		return 1
	}
	return 0
}

func serveTools(ctx context.Context, opts options, stdin io.Reader, stdout, stderr io.Writer, logger *slog.Logger) error {
	token := os.Getenv(authEnv)
	addr, err := httpAddress(opts.http, token)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(opts.config)
	if err != nil {
		return err
	}
	if cfg.UsesReadonly() {
		logger.Warn("config: readonly is deprecated and ignored: the server is always read-only")
	}
	store, err := spillStore(cfg, logger)
	if err != nil {
		return err
	}
	eng := engine.New(&cfg)
	defer eng.Close()
	go warmParser()
	server := tools.NewServer(tools.New(cfg, eng, store, stderr), buildVersion())
	if addr != "" {
		return serveHTTP(ctx, server, addr, token, logger)
	}
	err = server.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func loadConfig(path string) (config.Config, error) {
	if path == "" {
		return config.Config{}, errors.New("no config file: pass --config FILE, or set XDG_CONFIG_HOME or HOME to read the default one")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, validateSources(cfg)
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

func spillStore(cfg config.Config, logger *slog.Logger) (*spill.Store, error) {
	dir, err := spillDir()
	if err != nil {
		return nil, fmt.Errorf("spill directory: %w", err)
	}
	store := &spill.Store{
		Dir:            dir,
		TTL:            time.Duration(cfg.Defaults.SpillTTLHours) * time.Hour,
		InlineMaxBytes: config.InlineMaxBytes,
	}
	if err := store.Cleanup(); err != nil {
		logger.Warn("spill cleanup failed", slog.String("error", err.Error()))
	}
	return store, nil
}

func spillDir() (string, error) {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "pgglass"), nil
	}
	dir := filepath.Join(os.TempDir(), "pgglass-"+strconv.Itoa(os.Geteuid()))
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

func private(info fs.FileInfo, uid int) bool {
	owner, ok := fileOwner(info)
	return ok && owner == uid && info.IsDir() && info.Mode().Perm() == 0o700
}
