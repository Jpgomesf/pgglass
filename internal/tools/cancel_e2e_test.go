package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// A client that gives up on a call, as an agent does when its user
// interrupts it, sends notifications/cancelled: the server then ends the
// call, which cancels its query on Postgres at once, well before the
// statement timeout, logs the call and keeps the connection for the next one.
func TestCancellingACallCancelsItsQuery(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, appConns(t)))
	observer := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	const sql = "SELECT pg_sleep(60) AS cancelled_by_the_client"
	running := func() bool {
		return testdb.Value[bool](t, observer, "SELECT count(*) > 0 FROM pg_stat_activity WHERE query = $1 AND state = 'active'", sql)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := make(chan error, 1)
	go func() {
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "pg_query", Arguments: args("clean", sql)})
		called <- err
	}()
	eventually(t, "the query runs", running)

	cancelled := time.Now()
	cancel()
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Errorf("CallTool = %v, want context.Canceled", err)
	}
	eventually(t, "the query stops and the call is logged", func() bool {
		return !running() && strings.Contains(s.audit.String(), `"tool":"pg_query"`)
	})
	if took := time.Since(cancelled); took > 5*time.Second {
		t.Errorf("the query ran for %v after the client cancelled the call", took)
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Connection != "clean" || lines[0].Outcome != "db_error" {
		t.Errorf("audit lines = %+v, want one db_error on clean", lines)
	}
	if got := s.ok(t, "pg_query", args("clean", "SELECT 1 AS one")); got != "one\n1\n(1 row)" {
		t.Errorf("the next call = %q", got)
	}
}
