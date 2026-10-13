package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

func startApp(t *testing.T, conn map[string]any) httpServer {
	t.Helper()
	isolateHome(t)
	t.Setenv(authEnv, "")
	return startHTTP(t, writeConfig(t, connections(map[string]any{"app": conn})))
}

func awaitLog(t *testing.T, srv httpServer, fragment string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(srv.logs.String(), fragment); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr lacks %s after 5s:\n%s", fragment, srv.logs)
		}
	}
}

// Over Streamable HTTP, a client that gives up on a call drops its request:
// the server then ends the call, which cancels its query on Postgres at once,
// as over stdio, logs it and serves the next call. It used to run the query
// on until the statement timeout.
func TestHTTPCancellingACallCancelsItsQuery(t *testing.T) {
	srv := startApp(t, direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)))
	session, err := connectHTTP(t, srv.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := query(session, "SELECT 1 AS one"); got != "one\n1\n(1 row)" {
		t.Fatalf("pg_query = %q", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{
			"connection": "app", "sql": "SELECT pg_sleep(60) AS cancelled_over_http",
		}})
		called <- err
	}()
	awaitActiveQuery(t, "cancelled_over_http", true)
	cancelled := time.Now()
	cancel()
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Errorf("CallTool = %v, want context.Canceled", err)
	}
	awaitActiveQuery(t, "cancelled_over_http", false)
	if took := time.Since(cancelled); took > 5*time.Second {
		t.Errorf("the query ran for %v after the client cancelled the call", took)
	}
	awaitLog(t, srv, `"outcome":"db_error"`)
	if got := query(session, "SELECT 1 AS one"); got != "one\n1\n(1 row)" {
		t.Errorf("the next call = %q", got)
	}
}

// Over Streamable HTTP, which keeps no session, a confirm connection asks a
// client on the newest revision through the call's result, and runs the
// statement once the client accepts. The server cannot ask an older client
// in the middle of a call there, so it refuses that call and runs nothing.
func TestHTTPAsksBeforeRunningOnConfirmConnections(t *testing.T) {
	conn := direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))
	conn["confirm"] = true
	srv := startApp(t, conn)
	var (
		mu    sync.Mutex
		asked []string
	)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, req.Params.Message)
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	for _, version := range []string{mcp.SupportedProtocolVersions()[0], "2025-11-25"} {
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.url}, &mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		want := "one\n1\n(1 row)"
		if version != mcp.SupportedProtocolVersions()[0] {
			want = "error: pg_query on app requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation"
		}
		if got := query(session, "SELECT 1 AS one"); got != want {
			t.Errorf("%s: pg_query = %q, want %q", version, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Allow pg_query to run this statement on app?\n\nSELECT 1 AS one"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	if got := strings.Count(srv.logs.String(), `"outcome":"ok"`); got != 1 {
		t.Errorf("%d calls ran, want 1:\n%s", got, srv.logs)
	}
}

// Over Streamable HTTP, tables are listed and read as resources, as over
// stdio.
func TestHTTPServesTheTableResources(t *testing.T) {
	srv := startApp(t, direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)))
	session, err := connectHTTP(t, srv.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := session.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const uri = "pgglass://app/app/customers"
	if !slices.ContainsFunc(list.Resources, func(r *mcp.Resource) bool { return r.URI == uri }) {
		t.Errorf("resources/list lacks %s: %d resources", uri, len(list.Resources))
	}
	read, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.HasPrefix(read.Contents[0].Text, "app.customers: table, ~50 rows") {
		t.Errorf("resources/read %s = %+v", uri, read.Contents)
	}
	awaitLog(t, srv, `"tool":"resource","connection":"app"`)
}
