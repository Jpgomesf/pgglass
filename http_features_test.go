package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// Over Streamable HTTP, a dropped request cancels its query at once, as over
// stdio; the call is logged and the next one is served.
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

// Over HTTP, a confirm connection asks a client on the newest revision through
// the call's result, running once it accepts; an older client is refused.
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

// Over --http the server cannot ask an older client during the call, so an
// older client is refused as one without form elicitation is, audited, even
// when it claims form elicitation in _meta or in a batched initialize.
func TestHTTPRefusesOlderClientsThatClaimElicitation(t *testing.T) {
	conn := direct(unusedDSN)
	conn["confirm"] = true
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{%s"name":"pg_query","arguments":{"connection":"app","sql":"SELECT 1"}}}`
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{"elicitation":{}},"clientInfo":{"name":"test","version":"test"}}}`
	for name, tc := range map[string]struct{ version, body string }{
		"elicitation in _meta":        {"2025-06-18", fmt.Sprintf(call, `"_meta":{"io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}}}},`)},
		"elicitation in a batch init": {"2025-03-26", "[" + initialize + "," + fmt.Sprintf(call, "") + "]"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := startApp(t, conn)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.url, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", tc.version)
			res, err := http.DefaultClient.Do(req) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			const refusal = `"text":"pg_query on app requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation"}],"isError":true`
			if !strings.Contains(string(body), refusal) {
				t.Errorf("response lacks the refusal of a client that cannot ask:\n%s", body)
			}
			awaitLog(t, srv, `"tool":"pg_query","connection":"app"`)
			awaitLog(t, srv, `"outcome":"error"`)
		})
	}
}

// Over Streamable HTTP, tables are read and listed as resources, as over
// stdio.
func TestHTTPServesTheTableResources(t *testing.T) {
	srv := startApp(t, direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)))
	session, err := connectHTTP(t, srv.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	const uri = "pgglass://app/app/customers"
	read, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.HasPrefix(read.Contents[0].Text, "app.customers: table, ~50 rows") {
		t.Errorf("resources/read %s = %+v", uri, read.Contents)
	}
	awaitLog(t, srv, `"tool":"resource","connection":"app"`)
	list, err := session.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(list.Resources, func(r *mcp.Resource) bool { return r.URI == uri }) {
		t.Errorf("resources/list lacks %s: %d resources", uri, len(list.Resources))
	}
}

// A call still running when shutdown's 10 seconds run out is cancelled, with
// its query, so that the server stops then rather than when the query ends.
func TestHTTPShutdownCancelsCallsThatOutlastIt(t *testing.T) {
	srv := startApp(t, direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)))
	session, err := connectHTTP(t, srv.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := query(session, "SELECT 1 AS one"); got != "one\n1\n(1 row)" {
		t.Fatalf("pg_query = %q", got)
	}
	called := make(chan string, 1)
	go func() { called <- query(session, "SELECT 1 FROM pg_sleep(60) AS outlasts_the_shutdown") }()
	awaitActiveQuery(t, "outlasts_the_shutdown", true)
	start := time.Now()
	if code, took := srv.stop(), time.Since(start); code != 1 || took > shutdownTimeout+5*time.Second {
		t.Errorf("run = %d after %s, want 1 after about %s\nstderr:\n%s", code, took, shutdownTimeout, srv.logs)
	}
	awaitLog(t, srv, `"error":"shutdown: cancelled the calls still running after 10s"`)
	awaitActiveQuery(t, "outlasts_the_shutdown", false)
	if got := <-called; got != "error: query cancelled: context canceled" {
		t.Errorf("the call that outlasted the shutdown = %q, want it cancelled", got)
	}
}
