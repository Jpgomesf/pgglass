// Package engine runs read-only queries against the configured connections.
// It is the server's read-only boundary: each query runs alone in a READ ONLY
// transaction that is always rolled back, under timeouts and result limits,
// so nothing a query does outlives it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/pool"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

const (
	// deadlineGrace is how long past the statement timeout the client waits
	// before it gives up on the server.
	deadlineGrace = 2 * time.Second
	// cancelWait is how long a cancelled query has to stop on the server
	// before its connection is closed instead.
	cancelWait = 2 * time.Second
	// rollbackTimeout bounds the ROLLBACK that ends every query.
	rollbackTimeout = 5 * time.Second
	// vpnTimeout bounds the reachability check of requires_vpn connections.
	vpnTimeout = 3 * time.Second
)

var errClosed = errors.New("engine: manager is closed")

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

// Manager holds one connection pool per configured connection, opened on
// first use. It is safe for concurrent use.
type Manager struct {
	cfg    *config.Config
	limits Limits

	mu     sync.Mutex
	closed bool
	pools  map[string]*lazyPool
}

// lazyPool is the pool of one connection. Its own lock serializes opening
// it, so a slow connection never holds up the others.
type lazyPool struct {
	mu   sync.Mutex
	pool *pgxpool.Pool
}

// New returns a Manager for the connections in cfg. Limits come from
// cfg.Defaults.WithDefaults(): statement_timeout_seconds,
// lock_timeout_seconds, max_rows and max_bytes. DSNs are resolved lazily with
// resolver.Resolve when a connection is first used, so New does no I/O.
func New(cfg *config.Config) *Manager {
	d := cfg.Defaults.WithDefaults()
	return &Manager{
		cfg: cfg,
		limits: Limits{
			StatementTimeout: time.Duration(d.StatementTimeoutSeconds) * time.Second,
			LockTimeout:      time.Duration(d.LockTimeoutSeconds) * time.Second,
			MaxRows:          d.MaxRows,
			MaxBytes:         d.MaxBytes,
		},
		pools: make(map[string]*lazyPool),
	}
}

// Query runs q on the connection named conn inside a READ ONLY transaction
// that is always rolled back, with the Limits applied. Rows beyond MaxRows or
// MaxBytes are dropped and reported through Result.Truncated. Errors raised
// by Postgres are returned as *QueryError; connection, resolution and context
// failures as plain errors. Query does not run guard.Check: callers passing
// untrusted SQL must check it first.
func (m *Manager) Query(ctx context.Context, conn string, q Query) (*result.Result, error) {
	p, err := m.pool(ctx, conn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.limits.StatementTimeout+deadlineGrace)
	defer cancel()

	res, err := run(ctx, p, q, m.limits)
	if err == nil {
		err = nameTypes(ctx, p, res.Columns)
	}
	if err != nil {
		return nil, queryError(ctx, err)
	}
	return res, nil
}

// Querier returns a Querier bound to the connection named conn.
func (m *Manager) Querier(conn string) Querier {
	return boundQuerier{m: m, conn: conn}
}

// Close closes every pool, waiting for running queries to finish. The
// Manager must not be used afterwards: queries fail.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	pools := m.pools
	m.mu.Unlock()

	for _, lp := range pools {
		lp.mu.Lock()
		if lp.pool != nil {
			lp.pool.Close()
		}
		lp.mu.Unlock()
	}
}

type boundQuerier struct {
	m    *Manager
	conn string
}

func (b boundQuerier) Query(ctx context.Context, q Query) (*result.Result, error) {
	return b.m.Query(ctx, b.conn, q)
}

// pool returns the pool of the connection named name, opening it on first
// use. A failed open is retried by the next call.
func (m *Manager) pool(ctx context.Context, name string) (*pgxpool.Pool, error) {
	lp, err := m.lazyPool(name)
	if err != nil {
		return nil, err
	}
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if lp.pool == nil {
		// Close may have run since lazyPool returned; a pool opened now would leak.
		if m.isClosed() {
			return nil, errClosed
		}
		p, err := open(ctx, name, m.cfg.Connections[name])
		if err != nil {
			return nil, err
		}
		lp.pool = p
	}
	return lp.pool, nil
}

func (m *Manager) lazyPool(name string) (*lazyPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errClosed
	}
	if _, ok := m.cfg.Connections[name]; !ok {
		return nil, unknownConnection(name, m.cfg.Connections)
	}
	lp, ok := m.pools[name]
	if !ok {
		lp = &lazyPool{}
		m.pools[name] = lp
	}
	return lp, nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func unknownConnection(name string, conns map[string]config.Connection) error {
	if len(conns) == 0 {
		return fmt.Errorf("unknown connection %q: no connections are configured", name)
	}
	return fmt.Errorf("unknown connection %q (available: %s)", name, strings.Join(slices.Sorted(maps.Keys(conns)), ", "))
}

// open resolves the DSN of the connection named name and creates its pool,
// which dials on first use.
func open(ctx context.Context, name string, conn config.Connection) (*pgxpool.Pool, error) {
	dsn, err := resolver.Resolve(ctx, name, conn)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", name, err) // pgx redacts the password
	}
	if conn.RequiresVPN {
		cfg.BeforeConnect = func(_ context.Context, c *pgx.ConnConfig) error {
			return checkVPN(name, c.Host, int(c.Port))
		}
	}
	harden(cfg.ConnConfig)
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", name, err)
	}
	return p, nil
}

// checkVPN fails fast, before each dial, when the host of a requires_vpn
// connection is unreachable.
func checkVPN(name, host string, port int) error {
	if !pool.CheckVPN(host, port, vpnTimeout) {
		return fmt.Errorf("connection %q: host %s:%d unreachable — is VPN active?", name, host, port)
	}
	return nil
}

// harden sets what every connection needs, whatever the DSN says.
func harden(c *pgx.ConnConfig) {
	// A second layer under the READ ONLY transactions. PgBouncer refuses
	// startup parameters it does not know: list this one in its
	// track_extra_parameters (1.20+), or in ignore_startup_parameters.
	c.RuntimeParams["default_transaction_read_only"] = "on"
	// The extended protocol with the unnamed statement: one statement per
	// query, and no server-side state for a transaction-mode pooler to lose.
	c.DefaultQueryExecMode = pgx.QueryExecModeExec
	// A cancelled context cancels the query on the server and keeps the
	// connection for the next query; pgx's default drops the connection.
	c.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: cancelWait}
	}
}
