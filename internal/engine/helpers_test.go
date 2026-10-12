package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// db is the connection name newManager configures.
const db = "db"

// newManager returns a Manager whose connection db resolves to dsn. It is
// closed when t ends.
func newManager(t *testing.T, d config.Defaults, dsn string) *engine.Manager {
	t.Helper()
	return managerFor(t, d, map[string]config.Connection{db: {Source: "direct", DSN: dsn}})
}

// managerFor returns a Manager for conns with d, its unset settings at their
// defaults. It is closed when t ends.
func managerFor(t *testing.T, d config.Defaults, conns map[string]config.Connection) *engine.Manager {
	t.Helper()
	m := engine.New(&config.Config{Defaults: d.WithDefaults(), Connections: conns})
	t.Cleanup(m.Close)
	return m
}

// oneConn limits dsn's pool to one connection, so consecutive queries share
// a session.
func oneConn(dsn string) string { return dsn + "&pool_max_conns=1" }

func mustQuery(t *testing.T, m *engine.Manager, q engine.Query) *result.Result {
	t.Helper()
	res, err := m.Query(t.Context(), db, q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.SQL, err)
	}
	return res
}

// value returns the only value of the first row of a successful query.
func value(t *testing.T, m *engine.Manager, sql string) string {
	t.Helper()
	res := mustQuery(t, m, engine.Query{SQL: sql})
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		t.Fatalf("%s: no value in %+v", sql, res)
	}
	return deref(res.Rows[0][0])
}

func wantSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var qe *engine.QueryError
	if !errors.As(err, &qe) || qe.SQLState != state {
		t.Errorf("error = %v, want a *QueryError with SQLSTATE %s", err, state)
	}
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

// eventually fails t unless cond holds within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stub is the "stub" credential source. It resolves a connection's DSN
// field, counts calls per connection name and, for names with a gate, waits
// for the gate to open first. Tests use distinct connection names and call
// reset first, so repeated runs (-count) start clean.
var stub = &stubSource{calls: map[string]int{}, gates: map[string]chan struct{}{}}

func init() { resolver.RegisterSource(stub) }

type stubSource struct {
	mu    sync.Mutex
	calls map[string]int
	gates map[string]chan struct{}
}

func (*stubSource) Type() string { return "stub" }

func (*stubSource) Status(config.Connection) string { return "" }

func (s *stubSource) Resolve(ctx context.Context, name string, conn config.Connection) (string, error) {
	s.mu.Lock()
	s.calls[name]++
	gate := s.gates[name]
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return conn.DSN, nil
}

// reset forgets the calls and gate recorded for name and returns name.
func (s *stubSource) reset(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.calls, name)
	delete(s.gates, name)
	return name
}

func (s *stubSource) callCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

// hold makes Resolve for name wait until the returned function is called.
func (s *stubSource) hold(name string) (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gates[name] = gate
	s.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}
