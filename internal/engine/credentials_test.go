package engine_test

import (
	"context"
	"net/url"
	"sync"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/resolver"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

var cached = &cachedSource{}

func init() { resolver.RegisterSource(cached) }

type cachedSource struct {
	mu          sync.Mutex
	store       string
	cache       string
	fetches     int
	invalidated int
}

func (*cachedSource) Type() string { return "cached" }

func (*cachedSource) Status(config.Connection) string { return "" }

func (s *cachedSource) Resolve(context.Context, string, config.Connection) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == "" {
		s.cache = s.store
		s.fetches++
	}
	return s.cache, nil
}

func (s *cachedSource) Invalidate(config.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = ""
	s.invalidated++
}

func (s *cachedSource) reset(dsn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store, s.cache, s.fetches, s.invalidated = dsn, "", 0, 0
}

func (s *cachedSource) rotate(dsn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = dsn
}

func (s *cachedSource) wantCounts(t *testing.T, fetches, invalidated int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fetches != fetches || s.invalidated != invalidated {
		t.Errorf("%d fetches and %d invalidations, want %d and %d", s.fetches, s.invalidated, fetches, invalidated)
	}
}

// When the server refuses a cached password after a rotation, the engine has
// the source drop it and connects once more with credentials fetched afresh.
func TestRefusedCredentialsAreFetchedAgainOnce(t *testing.T) {
	const role = "engine_cached"
	admin := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("DROP ROLE IF EXISTS " + role)
	exec("CREATE ROLE " + role + " LOGIN PASSWORD 'first'")
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP ROLE "+role) })
	dsn := func(password string) string {
		u, err := url.Parse(testdb.DSN(t, testdb.RoleClean, testdb.DBApp) + "&pool_max_conn_lifetime=1ms")
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, password)
		return u.String()
	}

	cached.reset(dsn("first"))
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "cached"}})
	if got := value(t, m, "SELECT current_user"); got != role {
		t.Fatalf("current_user = %s, want %s", got, role)
	}
	cached.wantCounts(t, 1, 0)

	exec("ALTER ROLE " + role + " PASSWORD 'second'")
	cached.rotate(dsn("second"))
	if got := value(t, m, "SELECT current_user"); got != role {
		t.Errorf("after the rotation: current_user = %s, want %s", got, role)
	}
	cached.wantCounts(t, 2, 1)

	_, err := m.Query(t.Context(), db, engine.Query{SQL: "DO $$ BEGIN RAISE EXCEPTION 'raised' USING ERRCODE = '28P01'; END $$"})
	wantSQLState(t, err, "28P01")
	cached.wantCounts(t, 2, 1)

	exec("ALTER ROLE " + role + " PASSWORD 'third'")
	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "28P01")
	cached.wantCounts(t, 3, 2)

	exec("ALTER ROLE " + role + " NOLOGIN PASSWORD 'second'")
	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "28000")
	cached.wantCounts(t, 4, 3)
}

// A source that does not cache resolves the same credentials again, so a
// refused connection is not retried.
func TestRefusedCredentialsOfAnUncachedSourceAreNotRetried(t *testing.T) {
	name := stub.reset("refused-uncached")
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(testdb.RoleClean, "wrong")
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{name: {Source: "stub", DSN: u.String()}})

	_, err = m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "28P01")
	if n := stub.callCount(name); n != 2 {
		t.Errorf("resolved %d times, want 2", n)
	}
}
