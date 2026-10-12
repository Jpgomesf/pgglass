package engine_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// closedPort returns a local TCP port with nothing listening on it.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func unreachableDSN(t *testing.T) string {
	return "postgres://u:p@127.0.0.1:" + strconv.Itoa(closedPort(t)) + "/db?sslmode=disable&connect_timeout=2"
}

func TestNewResolvesNothingUntilFirstUse(t *testing.T) {
	name := stub.reset("lazy")
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{name: {Source: "stub", DSN: unreachableDSN(t)}})
	if n := stub.callCount(name); n != 0 {
		t.Fatalf("New resolved the DSN %d times, want 0", n)
	}

	// The first query opens the pool, which resolves once, and each attempt
	// to connect resolves again; the second query reuses the pool.
	for _, want := range []int{2, 3} {
		_, err := m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
		var qe *engine.QueryError
		if err == nil || errors.As(err, &qe) {
			t.Errorf("Query on a closed port: err = %v, want a plain connection error", err)
		}
		if n := stub.callCount(name); n != want {
			t.Errorf("resolved %d times, want %d", n, want)
		}
	}
}

// Each new database connection resolves the DSN again, so a rotated password
// reaches it without a restart. pool_max_conn_lifetime=1ms retires every
// connection after one use.
func TestEachNewConnectionResolvesTheDSNAgain(t *testing.T) {
	const role, envVar = "engine_rotating", "MCP_PG_ENGINE_ROTATING_DSN"
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

	t.Setenv(envVar, dsn("first"))
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "env", DSNEnvVar: envVar}})
	if got := value(t, m, "SELECT current_user"); got != role {
		t.Fatalf("current_user = %s, want %s", got, role)
	}

	exec("ALTER ROLE " + role + " PASSWORD 'second'")
	t.Setenv(envVar, dsn("second"))
	if got := value(t, m, "SELECT current_user"); got != role {
		t.Errorf("after the rotation: current_user = %s, want %s", got, role)
	}

	// The old password now fails: that query did connect anew.
	t.Setenv(envVar, dsn("first"))
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "28P01") // invalid_password
}

func TestUnknownConnection(t *testing.T) {
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{
		"staging": {Source: "direct", DSN: "postgres://localhost/s"},
		"prod":    {Source: "direct", DSN: "postgres://localhost/p"},
	})
	_, err := m.Query(t.Context(), "nope", engine.Query{SQL: "SELECT 1"})
	if want := `unknown connection "nope" (available: prod, staging)`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}

	empty := managerFor(t, config.Defaults{}, nil)
	_, err = empty.Query(t.Context(), "nope", engine.Query{SQL: "SELECT 1"})
	if want := `unknown connection "nope": no connections are configured`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestClosedManagerRefusesQueries(t *testing.T) {
	name := stub.reset("closed")
	m := engine.New(&config.Config{Connections: map[string]config.Connection{name: {Source: "stub", DSN: unreachableDSN(t)}}})
	m.Close()
	m.Close() // idempotent

	_, err := m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("err = %v, want a closed-manager error", err)
	}
	if n := stub.callCount(name); n != 0 {
		t.Errorf("a closed manager resolved the DSN %d times", n)
	}
}

// A query waiting to open a connection when Close runs must not open it
// afterwards: nothing would close that pool.
func TestQueryWaitingOnOpenDoesNotOpenAfterClose(t *testing.T) {
	name := stub.reset("open-after-close")
	release := stub.hold(name)
	defer release()
	// The first open fails after Resolve, leaving the connection unopened.
	m := engine.New(&config.Config{Connections: map[string]config.Connection{name: {Source: "stub", DSN: "postgres://u:p@localhost:notaport/db"}}})
	t.Cleanup(m.Close)

	query := func() <-chan error {
		errs := make(chan error, 1)
		go func() {
			_, err := m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
			errs <- err
		}()
		return errs
	}
	first := query()
	eventually(t, "the first query to start opening", func() bool { return stub.callCount(name) == 1 })
	second := query()
	time.Sleep(50 * time.Millisecond) // let the second query wait on the open

	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	eventually(t, "Close to start", func() bool {
		_, err := m.Query(t.Context(), "other", engine.Query{SQL: "SELECT 1"})
		return strings.Contains(err.Error(), "closed") // until then: unknown connection
	})
	release()
	<-closed

	if err := <-first; err == nil || !strings.Contains(err.Error(), "notaport") {
		t.Errorf("first query: err = %v, want the DSN error", err)
	}
	if err := <-second; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("second query: err = %v, want a closed-manager error", err)
	}
	if n := stub.callCount(name); n != 1 {
		t.Errorf("resolved %d times, want 1: the waiting query opened after Close", n)
	}
}

// A query waiting for another to open the connection gives up when its own
// context ends, instead of waiting out a slow resolve.
func TestQueryWaitingOnOpenGivesUpWithItsContext(t *testing.T) {
	name := stub.reset("open-wait-cancel")
	release := stub.hold(name)
	defer release()
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{name: {Source: "stub", DSN: "postgres://u:p@localhost:notaport/db"}})

	first := make(chan error, 1)
	go func() {
		_, err := m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
		first <- err
	}()
	eventually(t, "the first query to start opening", func() bool { return stub.callCount(name) == 1 })

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := m.Query(ctx, name, engine.Query{SQL: "SELECT 1"})
		waiting <- err
	}()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiting query: err = %v, want its context's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting query ignored its context")
	}

	release()
	if err := <-first; err == nil || !strings.Contains(err.Error(), "notaport") {
		t.Errorf("first query: err = %v, want the DSN error", err)
	}
}

func TestInvalidDSNErrorHidesPassword(t *testing.T) {
	m := newManager(t, config.Defaults{}, "postgres://app:s3cret-pw@localhost:notaport/app")
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), `connection "db"`) {
		t.Fatalf("err = %v, want a DSN error naming the connection", err)
	}
	if strings.Contains(err.Error(), "s3cret-pw") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestFailedResolveIsRetried(t *testing.T) {
	const envVar = "MCP_PG_ENGINE_TEST_DSN"
	t.Setenv(envVar, "")
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "env", DSNEnvVar: envVar}})

	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), envVar) {
		t.Fatalf("err = %v, want the unset variable reported", err)
	}

	t.Setenv(envVar, "postgres://u:p@localhost:notaport/db")
	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || strings.Contains(err.Error(), envVar) || !strings.Contains(err.Error(), "notaport") {
		t.Errorf("err = %v, want the new DSN parsed: a failed resolve must not stick", err)
	}
}

func TestRequiresVPNFailsFastWhenUnreachable(t *testing.T) {
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{
		db: {Source: "direct", DSN: unreachableDSN(t), RequiresVPN: true},
	})
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), "is VPN active?") {
		t.Errorf("err = %v, want the VPN hint", err)
	}
}

func TestRequiresVPNConnectsWhenReachable(t *testing.T) {
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{
		db: {Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, testdb.DBApp), RequiresVPN: true},
	})
	if v := value(t, m, "SELECT 1"); v != "1" {
		t.Errorf("SELECT 1 = %s", v)
	}
}

// Opening a connection holds only that connection's lock: queries on other
// connections proceed, and concurrent first uses open it once.
func TestOpeningOneConnectionBlocksNoOther(t *testing.T) {
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	slow, fast := stub.reset("slow-open"), stub.reset("fast-open")
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{
		slow: {Source: "stub", DSN: dsn},
		fast: {Source: "stub", DSN: dsn},
	})

	var wg sync.WaitGroup
	defer wg.Wait() // after release, on every path: the queries call t.Errorf
	release := stub.hold(slow)
	defer release()
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Query(t.Context(), slow, engine.Query{SQL: "SELECT 1"}); err != nil {
				t.Errorf("query on %s: %v", slow, err)
			}
		}()
	}
	eventually(t, "the first query to start opening "+slow, func() bool { return stub.callCount(slow) == 1 })

	done := make(chan error, 1)
	go func() {
		_, err := m.Query(t.Context(), fast, engine.Query{SQL: "SELECT 1"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("query on %s while %s is opening: %v", fast, slow, err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("query on %s waited for %s to open", fast, slow)
	}
	// The other first uses wait for that open instead of opening too.
	if n := stub.callCount(slow); n != 1 {
		t.Errorf("concurrent first uses resolved %s %d times while it was opening, want once", slow, n)
	}

	release()
	wg.Wait()
}
