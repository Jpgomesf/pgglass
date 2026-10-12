// Package engine runs read-only queries against the configured connections.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

var errNotImplemented = errors.New("not implemented")

// Limits bound every query.
type Limits struct {
	StatementTimeout time.Duration // SET LOCAL statement_timeout
	LockTimeout      time.Duration // SET LOCAL lock_timeout
	MaxRows          int           // rows kept before the result is truncated
	MaxBytes         int           // total bytes of cell values kept before the result is truncated
}

// Query is one SQL statement to run.
type Query struct {
	SQL    string
	Params []any // values for $1..$n
	// SearchPath, when set, names the one schema used as search_path for this
	// query only. The engine quotes it as an identifier.
	SearchPath string
}

// QueryError is an error reported by Postgres, flattened for display.
type QueryError struct {
	SQLState string
	Message  string
	Detail   string
	Hint     string
	Position int // 1-based character offset into the SQL; 0 when unknown
}

// Error returns the message and SQLSTATE.
func (e *QueryError) Error() string {
	return fmt.Sprintf("%s (SQLSTATE %s)", e.Message, e.SQLState)
}

// Querier runs queries on one connection.
type Querier interface {
	Query(ctx context.Context, q Query) (*result.Result, error)
}

// Manager holds one connection pool per configured connection. It is safe
// for concurrent use.
type Manager struct {
	cfg *config.Config
}

// New returns a Manager for the connections in cfg. Limits come from
// cfg.Defaults.WithDefaults(): statement_timeout_seconds,
// lock_timeout_seconds, max_rows and max_bytes. DSNs are resolved lazily with
// resolver.Resolve when a connection is first used, so New does no I/O.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// Query runs q on the connection named conn inside a READ ONLY transaction
// that is always rolled back, with the Limits applied. Rows beyond MaxRows or
// MaxBytes are dropped and reported through Result.Truncated. Errors raised
// by Postgres are returned as *QueryError; connection, resolution and context
// failures as plain errors. Query does not run guard.Check: callers passing
// untrusted SQL must check it first.
func (m *Manager) Query(ctx context.Context, conn string, q Query) (*result.Result, error) {
	return nil, errNotImplemented
}

// Querier returns a Querier bound to the connection named conn.
func (m *Manager) Querier(conn string) Querier {
	return boundQuerier{m: m, conn: conn}
}

// Close closes every pool. The Manager must not be used afterwards.
func (m *Manager) Close() {}

type boundQuerier struct {
	m    *Manager
	conn string
}

func (b boundQuerier) Query(ctx context.Context, q Query) (*result.Result, error) {
	return b.m.Query(ctx, b.conn, q)
}
