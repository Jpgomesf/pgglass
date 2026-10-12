// Package engine runs read-only queries against the configured connections.
// Each query runs alone in a READ ONLY transaction that is always rolled
// back, under timeouts and result limits, so nothing it does inside the
// transaction outlives it. What acts outside the transaction (dblink, server
// files and programs, session advisory locks) is for internal/guard to
// reject and for the connection's role to lack.
package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/guard"
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

// limits bound every query.
type limits struct {
	StatementTimeout time.Duration // SET LOCAL statement_timeout
	LockTimeout      time.Duration // SET LOCAL lock_timeout
	MaxRows          int           // rows kept before the result is truncated
	MaxBytes         int           // bytes kept before the result is truncated: each value's length plus valueCost
}

// Query is one SQL statement to run.
type Query struct {
	SQL    string
	Params []any // values for $1..$n
	// SearchPath, when set, names the one schema used as search_path for this
	// query only. The engine quotes it as an identifier, and refuses a schema
	// the guard denies, such as net.
	SearchPath string
	// MaxRows, when positive, lowers the max_rows limit for this query only.
	// It never raises it.
	MaxRows int
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
	limits limits

	mu     sync.Mutex
	closed bool
	pools  map[string]*lazyPool
}

// lazyPool is the pool of one connection. Its own lock serializes opening
// it, so a slow connection never holds up the others. The lock is a 1-slot
// channel so that a query waiting on a slow open can give up when its
// context ends.
type lazyPool struct {
	lock chan struct{}
	pool *pgxpool.Pool
}

// New returns a Manager for the connections in cfg. Its limits come from
// cfg.Defaults.WithDefaults(): statement_timeout_seconds,
// lock_timeout_seconds, max_rows and max_bytes. DSNs are resolved with
// resolver.Resolve when a connection is first used, and again for each new
// database connection, so New does no I/O.
func New(cfg *config.Config) *Manager {
	d := cfg.Defaults.WithDefaults()
	return &Manager{
		cfg: cfg,
		limits: limits{
			StatementTimeout: time.Duration(d.StatementTimeoutSeconds) * time.Second,
			LockTimeout:      time.Duration(d.LockTimeoutSeconds) * time.Second,
			MaxRows:          d.MaxRows,
			MaxBytes:         d.MaxBytes,
		},
		pools: make(map[string]*lazyPool),
	}
}

// Query runs q on the connection named conn inside a READ ONLY transaction
// that is always rolled back, with the limits applied. Rows beyond MaxRows or
// MaxBytes are dropped and reported through Result.Truncated. Errors raised
// by Postgres are returned as *QueryError; connection, resolution and context
// failures as plain errors. Query does not run guard.Check: callers passing
// untrusted SQL must check it first. It does refuse, with the
// *guard.Violation of guard.CheckSearchPath, a SearchPath that would let
// unqualified names reach a schema the guard denies.
func (m *Manager) Query(ctx context.Context, conn string, q Query) (*result.Result, error) {
	if err := guard.CheckSearchPath(q.SearchPath); err != nil {
		return nil, err
	}
	p, err := m.pool(ctx, conn)
	if err != nil {
		return nil, queryError(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, m.limits.StatementTimeout+deadlineGrace)
	defer cancel()

	res, err := run(ctx, p, q, m.limitsFor(q))
	if err == nil {
		err = nameTypes(ctx, p, res.Columns)
	}
	if err != nil {
		return nil, queryError(ctx, err)
	}
	return res, nil
}

// limitsFor returns the limits of q: the Manager's, with MaxRows lowered to
// q.MaxRows when that is smaller.
func (m *Manager) limitsFor(q Query) limits {
	l := m.limits
	if q.MaxRows > 0 && q.MaxRows < l.MaxRows {
		l.MaxRows = q.MaxRows
	}
	return l
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
		lp.lock <- struct{}{}
		if lp.pool != nil {
			lp.pool.Close()
		}
		<-lp.lock
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
	select {
	case lp.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-lp.lock }()
	if lp.pool == nil {
		// Close may have run since lazyPool returned; a pool opened now would leak.
		if m.isClosed() {
			return nil, errClosed
		}
		p, err := open(ctx, name, m.cfg.Connections[name], m.limits.MaxBytes)
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
		lp = &lazyPool{lock: make(chan struct{}, 1)}
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

// rowByteHeadroom is added to max_bytes to get the per-message cap. A row the
// budget allows is smaller than the budget on the wire, where a value takes 4
// bytes beyond its own, under valueCost; the headroom keeps the messages that
// are not rows, such as row descriptions and errors, readable when max_bytes
// is small.
const rowByteHeadroom = 1 << 20

// open resolves the DSN of the connection named name and creates its pool,
// which dials on first use. Each new database connection resolves the DSN
// again and takes its server, TLS settings and credentials from it, so a
// rotated password reaches it without a restart; the rest of the DSN, pool
// settings included, is read once, here. maxBytes is the result byte budget,
// used to bound one wire message.
func open(ctx context.Context, name string, conn config.Connection, maxBytes int) (*pgxpool.Pool, error) {
	cfg, err := parseDSN(ctx, name, conn)
	if err != nil {
		return nil, err
	}
	cfg.BeforeConnect = func(ctx context.Context, c *pgx.ConnConfig) error {
		fresh, err := parseDSN(ctx, name, conn)
		if err != nil {
			return err
		}
		f := fresh.ConnConfig
		c.Host, c.Port, c.TLSConfig, c.Fallbacks = f.Host, f.Port, f.TLSConfig, f.Fallbacks
		c.Database, c.User, c.Password = f.Database, f.User, f.Password
		if conn.RequiresVPN {
			return checkVPN(name, c.Host, int(c.Port))
		}
		return nil
	}
	harden(cfg.ConnConfig, maxBytes+rowByteHeadroom)
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", name, err)
	}
	return p, nil
}

// parseDSN resolves the DSN of the connection named name and parses it.
func parseDSN(ctx context.Context, name string, conn config.Connection) (*pgxpool.Config, error) {
	dsn, err := resolver.Resolve(ctx, name, conn)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", name, err) // pgx redacts the password
	}
	return cfg, nil
}

// checkVPN fails fast, before each dial, when the host of a requires_vpn
// connection is unreachable.
func checkVPN(name, host string, port int) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), vpnTimeout)
	if err != nil {
		return fmt.Errorf("connection %q: host %s:%d unreachable — is VPN active?", name, host, port)
	}
	_ = conn.Close()
	return nil
}

// harden sets what every connection needs, whatever the DSN says. maxBodyLen
// bounds one protocol message.
func harden(c *pgx.ConnConfig, maxBodyLen int) {
	// A second layer under the READ ONLY transactions. PgBouncer refuses
	// startup parameters it does not know: list this one in its
	// track_extra_parameters (1.20+), or in ignore_startup_parameters.
	c.RuntimeParams["default_transaction_read_only"] = "on"
	// The guard reads SQL as UTF-8, and results are decoded as UTF-8: a role
	// or database default such as SJIS, where a multibyte character can end
	// in a backslash byte, must not change how the server reads either.
	c.RuntimeParams["client_encoding"] = "UTF8"
	// The extended protocol with the unnamed statement: one statement per
	// query, and no server-side state for a transaction-mode pooler to lose.
	c.DefaultQueryExecMode = pgx.QueryExecModeExec
	// A cancelled context cancels the query on the server and keeps the
	// connection for the next query; pgx's default drops the connection.
	c.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: cancelWait}
	}
	// Bound one wire message so a single huge field (SELECT repeat('x', 1e9))
	// cannot be buffered far past the byte budget: pgx rejects a larger
	// message after reading its 5-byte header, before allocating the body. The
	// collector's MaxBytes cap only bounds the bytes it keeps, which is after
	// pgx has already buffered the whole row.
	c.AfterConnect = func(_ context.Context, pc *pgconn.PgConn) error {
		pc.Frontend().SetMaxBodyLen(maxBodyLen)
		return nil
	}
}
