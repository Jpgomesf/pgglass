// Package testdb gives integration tests a disposable Postgres server: one
// container per test binary, started on first use and terminated by Main.
// Every package that uses it needs
//
//	func TestMain(m *testing.M) { testdb.Main(m) }
//
// The server starts with two fixture databases (see fixtures/):
//
//   - DBApp: schema app with every column type, index, constraint and object
//     kind the server must handle, including a 20k-row table, RLS, a view, a
//     materialized view and the dblink extension.
//   - DBSupabase: a Supabase pg_net look-alike whose queue table and
//     net.http_post function PUBLIC can write and execute.
//
// Each role's password is its name. The image comes from MCP_PG_IMAGE
// (default DefaultImage). Callers are skipped under -short and fail, never
// skip, when Docker is unavailable.
package testdb

import (
	"context"
	_ "embed" // fixture SQL
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

// Roles and databases created by the fixtures.
const (
	Superuser    = "postgres"      // owns everything
	RoleClean    = "ro_clean"      // USAGE + SELECT on schema app, nothing else
	RoleLeaky    = "ro_leaky"      // RoleClean + INSERT on app.audit_log, EXECUTE on dblink, member of pg_read_server_files
	RoleSupabase = "ro_supabase"   // SELECT on public.profiles in DBSupabase; PUBLIC grants do the rest
	DBApp        = "app"           // the main fixture database
	DBSupabase   = "supabase_like" // the pg_net look-alike
)

// DefaultImage is the Postgres image used when MCP_PG_IMAGE is unset.
const DefaultImage = "postgres:17-alpine"

// maintenanceDB is the database the superuser connects to for cluster-wide statements.
const maintenanceDB = "postgres"

const startTimeout = 5 * time.Minute

var (
	//go:embed fixtures/roles.sql
	rolesSQL string
	//go:embed fixtures/app.sql
	appSQL string
	//go:embed fixtures/supabase_like.sql
	supabaseSQL string
)

var shared = &harness{start: startServer}

// Main runs the package's tests, then terminates the shared container.
func Main(m *testing.M) {
	shared.managed = true
	code := m.Run()
	if err := shared.terminate(); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: terminate container: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// DSN returns a URI that connects as role to database on the shared server,
// starting the server on first use.
func DSN(t testing.TB, role, database string) string {
	t.Helper()
	return shared.get(t).dsn(role, database)
}

// Querier returns an engine.Querier that reads database as role through the
// engine, with the default limits, as the server does. It is closed when t
// finishes.
func Querier(t testing.TB, role, database string) engine.Querier {
	t.Helper()
	m := engine.New(&config.Config{
		Defaults:    config.Defaults{}.WithDefaults(),
		Connections: map[string]config.Connection{"db": {Source: "direct", DSN: DSN(t, role, database)}},
	})
	t.Cleanup(m.Close)
	return m.Querier("db")
}

// Conn opens a plain connection to database as role, outside the engine, for
// setup and to observe what queries leave behind. It is closed when t
// finishes.
func Conn(t testing.TB, role, database string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), DSN(t, role, database))
	if err != nil {
		t.Fatalf("testdb: connect as %s to %s: %v", role, database, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// Value runs sql on conn and returns the one value it selects.
func Value[T any](t testing.TB, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := conn.QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("testdb: %s: %v", sql, err)
	}
	return v
}

// CreateDatabase creates database name, runs fixtureSQL in it as the superuser
// and drops the database, closing any open connections, when t finishes.
// fixtureSQL may hold several statements. Roles are cluster-wide and survive
// the drop, so create them idempotently and give each the password of its
// name to reach it through DSN.
func CreateDatabase(t testing.TB, name, fixtureSQL string) {
	t.Helper()
	srv := shared.get(t)
	if err := srv.createDatabase(t.Context(), name); err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.dropDatabase(context.Background(), name); err != nil {
			t.Errorf("testdb: %v", err)
		}
	})
	if err := srv.loadFixture(t.Context(), name, fixtureSQL); err != nil {
		t.Fatalf("testdb: %v", err)
	}
}

// harness starts the server once per test binary.
type harness struct {
	start   func(context.Context) (*server, error)
	managed bool // set by Main, which terminates the container

	once sync.Once
	srv  *server
	err  error
}

func (h *harness) get(t testing.TB) *server {
	t.Helper()
	if testing.Short() {
		t.Skip("testdb: integration test skipped under -short")
	}
	if !h.managed {
		t.Fatal("testdb: call testdb.Main(m) from TestMain so the container is terminated")
	}
	h.once.Do(func() { h.srv, h.err = h.start(context.Background()) })
	if h.err != nil {
		t.Fatalf("testdb: start Postgres container (is Docker running?): %v", h.err)
	}
	return h.srv
}

func (h *harness) terminate() error {
	if h.srv == nil {
		return nil
	}
	return testcontainers.TerminateContainer(h.srv.ctr)
}

// Image returns the Postgres image to run: MCP_PG_IMAGE, or DefaultImage.
func Image() string {
	if img := os.Getenv("MCP_PG_IMAGE"); img != "" {
		return img
	}
	return DefaultImage
}

type server struct {
	ctr  *postgres.PostgresContainer
	host string
	port string
}

// startServer runs the container and loads the fixtures, terminating the
// container if any step fails.
func startServer(ctx context.Context) (srv *server, err error) {
	// testcontainers panics, rather than failing, when it finds no Docker host.
	defer func() {
		if r := recover(); r != nil {
			srv, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	ctr, err := postgres.Run(ctx, Image(),
		postgres.WithDatabase(maintenanceDB),
		postgres.WithUsername(Superuser),
		postgres.WithPassword(Superuser),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, errors.Join(err, testcontainers.TerminateContainer(ctr))
	}
	srv, err = newServer(ctx, ctr)
	if err == nil {
		err = srv.loadFixtures(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, testcontainers.TerminateContainer(ctr))
	}
	return srv, nil
}

func newServer(ctx context.Context, ctr *postgres.PostgresContainer) (*server, error) {
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, fmt.Errorf("container port: %w", err)
	}
	return &server{ctr: ctr, host: host, port: port.Port()}, nil
}

func (s *server) loadFixtures(ctx context.Context) error {
	if err := s.exec(ctx, maintenanceDB, rolesSQL); err != nil {
		return fmt.Errorf("create roles: %w", err)
	}
	for _, db := range []struct{ name, sql string }{{DBApp, appSQL}, {DBSupabase, supabaseSQL}} {
		if err := s.createDatabase(ctx, db.name); err != nil {
			return err
		}
		if err := s.loadFixture(ctx, db.name, db.sql); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) dsn(role, database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, role),
		Host:     net.JoinHostPort(s.host, s.port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func (s *server) createDatabase(ctx context.Context, name string) error {
	if err := s.exec(ctx, maintenanceDB, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

func (s *server) loadFixture(ctx context.Context, database, fixtureSQL string) error {
	if err := s.exec(ctx, database, fixtureSQL); err != nil {
		return fmt.Errorf("load fixture into %s: %w", database, err)
	}
	return nil
}

func (s *server) dropDatabase(ctx context.Context, name string) error {
	if err := s.exec(ctx, maintenanceDB, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// exec runs sql, which may hold several statements, as the superuser on database.
func (s *server) exec(ctx context.Context, database, sql string) error {
	conn, err := pgx.Connect(ctx, s.dsn(Superuser, database))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}
