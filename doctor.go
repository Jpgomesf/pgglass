package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/resolver"
)

const connectTimeout = 5 * time.Second

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "FAIL"
)

type check struct {
	name   string
	report preflight.Report
	err    error
}

func doctor(ctx context.Context, path string, stdout, stderr io.Writer) int {
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(stderr, "pgglass doctor: %v\n", err)
		return 1
	}
	report, failed := summarize(path, cfg, checkAll(ctx, cfg))
	_, _ = stdout.Write([]byte(report))
	if failed {
		return 1
	}
	return 0
}

func checkAll(ctx context.Context, cfg config.Config) []check {
	eng := engine.New(&cfg)
	defer eng.Close()
	names := slices.Sorted(maps.Keys(cfg.Connections))
	checks := make([]check, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			report, err := checkConnection(ctx, eng, name, cfg.Connections[name])
			checks[i] = check{name: name, report: report, err: err}
		})
	}
	wg.Wait()
	return checks
}

func summarize(path string, cfg config.Config, checks []check) (string, bool) {
	var b strings.Builder
	b.WriteString("config " + path + " (role_check " + cfg.Defaults.RoleCheck + ")\n")
	if cfg.UsesReadonly() {
		b.WriteString(statusWarn + " config: readonly is deprecated and ignored: the server is always read-only\n")
	}
	failed := 0
	for _, c := range checks {
		status, detail := verdict(c.report, c.err)
		if status == statusFail {
			failed++
		}
		b.WriteString(status + " " + c.name + ": " + detail + "\n")
	}
	b.WriteString(strconv.Itoa(failed) + " failed, " + strconv.Itoa(len(checks)-failed) + " passed\n")
	return b.String(), failed > 0
}

func checkConnection(ctx context.Context, eng *engine.Manager, name string, conn config.Connection) (preflight.Report, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	dsn, err := resolver.Resolve(connectCtx, name, conn)
	if err != nil {
		return preflight.Report{}, fmt.Errorf("credentials: %w", err)
	}
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return preflight.Report{}, errors.New("credentials: the DSN does not parse (not shown: it may hold a password)")
	}
	q := eng.Querier(name)
	if _, err := q.Query(connectCtx, engine.Query{SQL: "SELECT 1"}); err != nil {
		if errors.Is(connectCtx.Err(), context.DeadlineExceeded) {
			return preflight.Report{}, fmt.Errorf("connect: no answer within %s", connectTimeout)
		}
		return preflight.Report{}, fmt.Errorf("connect: %w", err)
	}
	report, err := preflight.Audit(ctx, q)
	if err != nil {
		return preflight.Report{}, fmt.Errorf("role check: %w", err)
	}
	return report, nil
}

func verdict(report preflight.Report, err error) (status, detail string) {
	switch {
	case err != nil:
		return statusFail, strings.ReplaceAll(err.Error(), "\n", "\n  ")
	case report.Blocking():
		return statusFail, report.String()
	case len(report.Findings) > 0:
		return statusWarn, report.String()
	default:
		return statusOK, report.String()
	}
}
