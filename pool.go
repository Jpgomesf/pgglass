package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type poolEntry struct {
	conn      *pgx.Conn
	createdAt time.Time
}

// ConnPool is a simple connection cache with TTL. Thread-safe.
type ConnPool struct {
	mu    sync.Mutex
	conns map[string]*poolEntry
	ttl   time.Duration
}

var pool = &ConnPool{
	conns: make(map[string]*poolEntry),
	ttl:   DefaultConnectionTTL,
}

// Get returns a cached connection if available and alive, nil otherwise.
func (p *ConnPool) Get(ctx context.Context, connName string) *pgx.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.conns[connName]
	if !ok {
		return nil
	}

	if time.Since(entry.createdAt) > p.ttl {
		p.evict(ctx, connName, entry.conn)
		return nil
	}

	if err := entry.conn.Ping(ctx); err != nil {
		p.evict(ctx, connName, entry.conn)
		return nil
	}

	return entry.conn
}

// Put stores a connection in the pool.
func (p *ConnPool) Put(ctx context.Context, connName string, conn *pgx.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if old, ok := p.conns[connName]; ok {
		p.evict(ctx, connName, old.conn)
	}
	p.conns[connName] = &poolEntry{conn: conn, createdAt: time.Now()}
}

func (p *ConnPool) evict(ctx context.Context, k string, conn *pgx.Conn) {
	delete(p.conns, k)
	_ = conn.Close(ctx)
}

// CheckVPN tests if a host:port is reachable within the timeout.
func CheckVPN(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// OpenConnection returns a pooled or new pgx connection for the named connection.
func OpenConnection(ctx context.Context, connName string) (*pgx.Conn, error) {
	if cached := pool.Get(ctx, connName); cached != nil {
		return cached, nil
	}

	connCfg, ok := AppConfig.Connections[connName]
	if !ok {
		return nil, fmt.Errorf("unknown connection %q (available: %s)", connName, strings.Join(connNames(), ", "))
	}

	dsn, err := ResolveCredentials(ctx, connName, connCfg)
	if err != nil {
		return nil, err
	}

	if connCfg.RequiresVPN {
		host, port := extractHostPort(dsn)
		if host != "" && !CheckVPN(host, port, 3*time.Second) {
			return nil, fmt.Errorf("connection %q: host %s:%d unreachable — is VPN active?", connName, host, port)
		}
	}

	// Append statement timeout
	dsn = appendDSNParam(dsn, "statement_timeout", strconv.Itoa(AppConfig.StatementTimeoutMS()))

	// Enforce read-only if configured
	if connCfg.IsReadonly(AppConfig.Defaults) {
		dsn = appendDSNParam(dsn, "default_transaction_read_only", "on")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w", connName, err)
	}

	pool.Put(ctx, connName, conn)
	return conn, nil
}

// ReturnConnection rolls back any pending transaction state.
func ReturnConnection(ctx context.Context, conn *pgx.Conn) {
	_, _ = conn.Exec(ctx, "ROLLBACK")
}

// appendDSNParam adds a key=value parameter to a DSN string.
// Handles both URI format (postgres://...) and keyword/value format (host=... port=...).
func appendDSNParam(dsn, key, value string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + key + "=" + value
	}
	return dsn + " " + key + "=" + value
}

// extractHostPort parses host and port from a DSN (URI or keyword/value format).
func extractHostPort(dsn string) (string, int) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", 0
		}
		host := u.Hostname()
		port := 5432
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
		return host, port
	}

	// keyword/value format: host=... port=...
	var host string
	port := 5432
	for _, part := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "host":
			host = v
		case "port":
			port, _ = strconv.Atoi(v)
		}
	}
	return host, port
}
