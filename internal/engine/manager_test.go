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

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

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
// reaches it without a restart.
func TestEachNewConnectionResolvesTheDSNAgain(t *testing.T) {
	const role, envVar = "engine_rotating", "PGGLASS_ENGINE_ROTATING_DSN"
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

	t.Setenv(envVar, dsn("first"))
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	wantSQLState(t, err, "28P01")
}

// A DSN that comes to name another user, database or host fails each new
// connection instead, until a restart: the role check audited the first.
func TestNewConnectionRefusesAnotherUserDatabaseOrHost(t *testing.T) {
	const envVar = "PGGLASS_ENGINE_TARGET_DSN"
	oneUse := func(dsn string) string { return dsn + "&pool_max_conn_lifetime=1ms" }
	first := oneUse(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))
	t.Setenv(envVar, first)
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "env", DSNEnvVar: envVar}})
	if got := value(t, m, "SELECT current_user"); got != testdb.RoleClean {
		t.Fatalf("current_user = %s, want %s", got, testdb.RoleClean)
	}

	for what, dsn := range map[string]string{
		"user":     testdb.DSN(t, testdb.Superuser, testdb.DBApp),
		"database": testdb.DSN(t, testdb.RoleClean, testdb.DBSupabase),
		"host":     "postgres://ro_clean:ro_clean@127.0.0.1:" + strconv.Itoa(closedPort(t)) + "/app?sslmode=disable",
	} {
		t.Setenv(envVar, oneUse(dsn))
		_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT current_user"})
		if err == nil || !strings.Contains(err.Error(), "names another user, database or host; restart the server") {
			t.Errorf("another %s: err = %v, want the connection refused until a restart", what, err)
		}
	}

	t.Setenv(envVar, first)
	if got := value(t, m, "SELECT current_user"); got != testdb.RoleClean {
		t.Errorf("first DSN again: current_user = %s, want %s", got, testdb.RoleClean)
	}
}

// A DSN that stops resolving fails each new connection with the source's
// error, until it resolves again.
func TestNewConnectionFailsWhileTheDSNDoesNotResolve(t *testing.T) {
	const envVar = "PGGLASS_ENGINE_VANISHING_DSN"
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp) + "&pool_max_conn_lifetime=1ms"
	t.Setenv(envVar, dsn)
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "env", DSNEnvVar: envVar}})
	if got := value(t, m, "SELECT current_user"); got != testdb.RoleClean {
		t.Fatalf("current_user = %s, want %s", got, testdb.RoleClean)
	}

	t.Setenv(envVar, "")
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), `environment variable "`+envVar+`" is not set`) {
		t.Errorf("err = %v, want the source's error", err)
	}

	t.Setenv(envVar, dsn)
	if got := value(t, m, "SELECT current_user"); got != testdb.RoleClean {
		t.Errorf("once the DSN resolves again: current_user = %s, want %s", got, testdb.RoleClean)
	}
}

// A Querier runs on the connection it is bound to.
func TestQuerierRunsOnItsConnection(t *testing.T) {
	t.Parallel()
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{
		"app":      {Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, testdb.DBApp)},
		"supabase": {Source: "direct", DSN: testdb.DSN(t, testdb.RoleSupabase, testdb.DBSupabase)},
	})
	for conn, want := range map[string]string{"app": "ro_clean app", "supabase": "ro_supabase supabase_like"} {
		res, err := m.Querier(conn).Query(t.Context(), engine.Query{SQL: "SELECT current_user || ' ' || current_database()"})
		if err != nil {
			t.Errorf("Querier(%s): %v", conn, err)
			continue
		}
		if got := deref(res.Rows[0][0]); got != want {
			t.Errorf("Querier(%s) ran as %q, want %q", conn, got, want)
		}
	}
	_, err := m.Querier("nope").Query(t.Context(), engine.Query{SQL: "SELECT 1"})
	if want := `unknown connection "nope" (available: app, supabase)`; err == nil || err.Error() != want {
		t.Errorf("Querier(nope): err = %v, want %q", err, want)
	}
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
	m.Close()

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
	eventually(t, 5*time.Second, "the first query to start opening", func() bool { return stub.callCount(name) == 1 })
	second := query()
	time.Sleep(50 * time.Millisecond)

	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	eventually(t, 5*time.Second, "Close to start", func() bool {
		_, err := m.Query(t.Context(), "other", engine.Query{SQL: "SELECT 1"})
		return strings.Contains(err.Error(), "closed")
	})
	release()
	<-closed

	if err := <-first; err == nil || !strings.Contains(err.Error(), "the DSN does not parse") {
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
	eventually(t, 5*time.Second, "the first query to start opening", func() bool { return stub.callCount(name) == 1 })

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
	if err := <-first; err == nil || !strings.Contains(err.Error(), "the DSN does not parse") {
		t.Errorf("first query: err = %v, want the DSN error", err)
	}
}

// A DSN that does not parse is not shown: pgx hides the password of only the
// DSNs it can read, not of a JSON secret or a token put where a DSN goes.
func TestUnparsableDSNIsNotShown(t *testing.T) {
	const secret = "s3cret-pw"
	for _, dsn := range []string{
		"postgres://app:" + secret + "@localhost:notaport/app",
		"host=localhost port=notaport user=app password=" + secret,
		`{"username":"app","password":"` + secret + `","host":"db"}`,
		secret,
	} {
		m := newManager(t, config.Defaults{}, dsn)
		_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
		if want := `connection "db": the DSN does not parse (not shown: it may hold a password)`; err == nil || err.Error() != want {
			t.Errorf("DSN %q: err = %v, want %q", dsn, err, want)
		}
	}
}

func TestFailedResolveIsRetried(t *testing.T) {
	const envVar = "PGGLASS_ENGINE_TEST_DSN"
	t.Setenv(envVar, "")
	m := managerFor(t, config.Defaults{}, map[string]config.Connection{db: {Source: "env", DSNEnvVar: envVar}})

	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), envVar) {
		t.Fatalf("err = %v, want the unset variable reported", err)
	}

	t.Setenv(envVar, "postgres://u:p@localhost:notaport/db")
	_, err = m.Query(t.Context(), db, engine.Query{SQL: "SELECT 1"})
	if err == nil || strings.Contains(err.Error(), envVar) || !strings.Contains(err.Error(), "the DSN does not parse") {
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
	defer wg.Wait()
	release := stub.hold(slow)
	defer release()
	for range 5 {
		wg.Go(func() {
			if _, err := m.Query(t.Context(), slow, engine.Query{SQL: "SELECT 1"}); err != nil {
				t.Errorf("query on %s: %v", slow, err)
			}
		})
	}
	eventually(t, 5*time.Second, "the first query to start opening "+slow, func() bool { return stub.callCount(slow) == 1 })

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
	if n := stub.callCount(slow); n != 1 {
		t.Errorf("concurrent first uses resolved %s %d times while it was opening, want once", slow, n)
	}

	release()
	wg.Wait()
}
