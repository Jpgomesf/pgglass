package engine_test

import (
	"errors"
	"net"
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

	for range 2 {
		_, err := m.Query(t.Context(), name, engine.Query{SQL: "SELECT 1"})
		var qe *engine.QueryError
		if err == nil || errors.As(err, &qe) {
			t.Errorf("Query on a closed port: err = %v, want a plain connection error", err)
		}
	}
	if n := stub.callCount(name); n != 1 {
		t.Errorf("resolved %d times over two queries, want once: the pool is kept", n)
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

	release()
	wg.Wait()
	if n := stub.callCount(slow); n != 1 {
		t.Errorf("%s resolved %d times by concurrent first uses, want once", slow, n)
	}
}
