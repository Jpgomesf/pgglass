package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/guard"
)

const (
	outcomeOK             = "ok"
	outcomeGuardReject    = "guard_reject"
	outcomeDBError        = "db_error"
	outcomePreflightBlock = "preflight_block"
	outcomeError          = "error"
	outcomeDeclined       = "declined"
)

type call struct {
	tool       string
	start      time.Time
	connection string
	sql        string
	rows       int
	truncated  bool
	notice     string
	audited    bool
	answers    mcp.InputResponseMap
	canAsk     bool
}

func (h *Handlers) audit(c *call, err error) {
	outcome, sqlState := classify(err)
	h.log.Info("tool call",
		slog.String("tool", c.tool),
		slog.String("connection", c.connection),
		slog.String("sql_sha256", sqlHash(c.sql)),
		slog.Int("sql_len", len(c.sql)),
		slog.Int64("duration_ms", time.Since(c.start).Milliseconds()),
		slog.Int("rows", c.rows),
		slog.Bool("truncated", c.truncated),
		slog.String("outcome", outcome),
		slog.String("sqlstate", sqlState),
	)
}

func sqlHash(sql string) string {
	if sql == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:6])
}

type dbError struct{ err error }

func (e *dbError) Error() string { return e.err.Error() }

func (e *dbError) Unwrap() error { return e.err }

type connError struct {
	conn string
	err  error
}

func (e *connError) Error() string { return e.conn + ": " + e.err.Error() }

func (e *connError) Unwrap() error { return e.err }

func classify(err error) (outcome, sqlState string) {
	var (
		violation *guard.Violation
		blocked   *blockedError
		dbErr     *dbError
		queryErr  *engine.QueryError
	)
	switch {
	case err == nil:
		return outcomeOK, ""
	case errors.As(err, &violation):
		return outcomeGuardReject, ""
	case errors.As(err, &blocked):
		return outcomePreflightBlock, ""
	case errors.As(err, &queryErr):
		return outcomeDBError, queryErr.SQLState
	case errors.As(err, &dbErr):
		return outcomeDBError, ""
	case errors.Is(err, errDeclined):
		return outcomeDeclined, ""
	default:
		return outcomeError, ""
	}
}

func errorText(err error) string {
	var (
		connErr   *connError
		checkErr  *roleCheckError
		violation *guard.Violation
		queryErr  *engine.QueryError
	)
	switch {
	case errors.As(err, &connErr):
		return connErr.conn + ": " + errorText(connErr.err)
	case errors.As(err, &checkErr):
		return roleCheckFailed + errorText(checkErr.err)
	case errors.As(err, &violation):
		return "query rejected: " + violation.Reason
	case errors.As(err, &queryErr):
		return queryErrorText(queryErr)
	default:
		return err.Error()
	}
}

func queryErrorText(e *engine.QueryError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ERROR: %s (SQLSTATE %s)", e.Message, e.SQLState)
	if e.Position > 0 {
		fmt.Fprintf(&b, " at character %d", e.Position)
	}
	if e.Detail != "" {
		b.WriteString("\nDETAIL: " + e.Detail)
	}
	if e.Hint != "" {
		b.WriteString("\nHINT: " + e.Hint)
	}
	return b.String()
}
