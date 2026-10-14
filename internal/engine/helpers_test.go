package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/resolver"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const db = "db"

func newManager(t *testing.T, d config.Defaults, dsn string) *engine.Manager {
	t.Helper()
	return managerFor(t, d, map[string]config.Connection{db: {Source: "direct", DSN: dsn}})
}

func managerFor(t *testing.T, d config.Defaults, conns map[string]config.Connection) *engine.Manager {
	t.Helper()
	m := engine.New(&config.Config{Defaults: d.WithDefaults(), Connections: conns})
	t.Cleanup(m.Close)
	return m
}

func oneConn(dsn string) string { return dsn + "&pool_max_conns=1" }

func mustQuery(t *testing.T, m *engine.Manager, q engine.Query) *result.Result {
	t.Helper()
	res, err := m.Query(t.Context(), db, q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.SQL, err)
	}
	return res
}

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

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

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

func (s *stubSource) hold(name string) (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gates[name] = gate
	s.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}
