// Package engine runs read-only queries against the configured connections.
// Each query runs alone in a READ ONLY transaction that is always rolled
// back, under timeouts and result limits: Postgres refuses its table writes,
// and the rollback undoes what else it changes transactionally. Neither
// reaches what acts outside the transaction, writes in place or takes a
// transaction id that no rollback gives back: internal/guard rejects the
// functions that do (see its denylists), and the connection's role should
// lack most of them.
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

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/resolver"
	"github.com/jpgomesf/pgglass/internal/result"
)

const (
	deadlineGrace   = 2 * time.Second
	cancelWait      = 2 * time.Second
	rollbackTimeout = 5 * time.Second
	vpnTimeout      = 3 * time.Second
)

var errClosed = errors.New("engine: manager is closed")

type limits struct {
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	MaxRows          int
	MaxBytes         int
}

// Query is one SQL statement to run.
type Query struct {
	SQL    string
	Params []any
	// SearchPath, when set, names the one schema used as search_path for this
	// query only. The engine quotes it as an identifier.
	SearchPath string
	// MaxRows, when positive, lowers the max_rows limit for this query only.
	// It never raises it.
	MaxRows int
	// MaxBytes, when larger than max_bytes, raises it for this query only, and
	// with it the size of a server message the query reads: for a value read
	// only to be summarized, such as a plan. It never lowers it.
	MaxBytes int
}

// WholeValueMaxBytes is the MaxBytes of a query that reads one value whole
// only to summarize it, or to render it and spill what is too large, such as
// a plan: 16 MiB.
const WholeValueMaxBytes = 16 << 20

// QueryError is an error reported by Postgres, flattened for display.
type QueryError struct {
	SQLState string
	Message  string
	Detail   string
	Hint     string
	Position int
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

type lazyPool struct {
	lock chan struct{}
	pool *pgxpool.Pool
}

// New returns a Manager for cfg, limited by its Defaults as config.Parse fills
// them. It does no I/O: DSNs resolve on first use and for each new connection.
func New(cfg *config.Config) *Manager {
	d := cfg.Defaults
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

// Query runs q on conn within the limits, in a READ ONLY transaction it always
// rolls back. It runs no guard: check untrusted SQL and SearchPath first.
func (m *Manager) Query(ctx context.Context, conn string, q Query) (*result.Result, error) {
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

func (m *Manager) limitsFor(q Query) limits {
	l := m.limits
	if q.MaxRows > 0 && q.MaxRows < l.MaxRows {
		l.MaxRows = q.MaxRows
	}
	l.MaxBytes = max(l.MaxBytes, q.MaxBytes)
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

func (m *Manager) pool(ctx context.Context, name string) (connPool, error) {
	lp, err := m.lazyPool(name)
	if err != nil {
		return connPool{}, err
	}
	select {
	case lp.lock <- struct{}{}:
	case <-ctx.Done():
		return connPool{}, ctx.Err()
	}
	defer func() { <-lp.lock }()
	if lp.pool == nil {
		if m.isClosed() {
			return connPool{}, errClosed
		}
		p, err := open(ctx, name, m.cfg.Connections[name], m.limits.MaxBytes)
		if err != nil {
			return connPool{}, err
		}
		lp.pool = p
	}
	return connPool{pool: lp.pool, conn: m.cfg.Connections[name]}, nil
}

type connPool struct {
	pool *pgxpool.Pool
	conn config.Connection
}

func (p connPool) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	c, err := p.pool.Acquire(ctx)
	if authFailed(err) && resolver.Invalidate(p.conn) {
		c, err = p.pool.Acquire(ctx)
	}
	return c, err
}

func authFailed(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000")
}

func (m *Manager) lazyPool(name string) (*lazyPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errClosed
	}
	if _, ok := m.cfg.Connections[name]; !ok {
		return nil, UnknownConnection(name, m.cfg.Connections)
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

// UnknownConnection is the error for a connection name that is not in conns.
func UnknownConnection(name string, conns map[string]config.Connection) error {
	if len(conns) == 0 {
		return fmt.Errorf("unknown connection %q: no connections are configured", name)
	}
	return fmt.Errorf("unknown connection %q (available: %s)", name, strings.Join(slices.Sorted(maps.Keys(conns)), ", "))
}

const rowByteHeadroom = 1 << 20

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
		if target(&f.Config) != target(&c.Config) {
			return fmt.Errorf("connection %q: the DSN now names another user, database or host; restart the server to use it, so that its role is checked", name)
		}
		c.Host, c.Port, c.TLSConfig, c.Fallbacks, c.Password = f.Host, f.Port, f.TLSConfig, f.Fallbacks, f.Password
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

func target(c *pgconn.Config) string {
	hosts := []string{net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port)))}
	for _, f := range c.Fallbacks {
		hosts = append(hosts, net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port))))
	}
	slices.Sort(hosts)
	return fmt.Sprintf("%q %q %q", c.User, c.Database, slices.Compact(hosts))
}

func parseDSN(ctx context.Context, name string, conn config.Connection) (*pgxpool.Config, error) {
	dsn, err := resolver.Resolve(ctx, name, conn)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("connection %q: the DSN does not parse (not shown: it may hold a password)", name)
	}
	return cfg, nil
}

func checkVPN(name, host string, port int) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), vpnTimeout)
	if err != nil {
		return fmt.Errorf("connection %q: host %s:%d unreachable — is VPN active?", name, host, port)
	}
	_ = conn.Close()
	return nil
}

func harden(c *pgx.ConnConfig, maxBodyLen int) {
	c.RuntimeParams["default_transaction_read_only"] = "on"
	c.RuntimeParams["client_encoding"] = "UTF8"
	c.DefaultQueryExecMode = pgx.QueryExecModeExec
	c.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: cancelWait}
	}
	c.AfterConnect = func(_ context.Context, pc *pgconn.PgConn) error {
		pc.Frontend().SetMaxBodyLen(maxBodyLen)
		return nil
	}
}
