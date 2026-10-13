package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

var testToken = rand.Text()

func TestHTTPAddress(t *testing.T) {
	for _, tc := range []struct {
		flag, token, want, err string
	}{
		{"", "", "", ""},
		{":8411", "", "127.0.0.1:8411", ""},
		{":", "", "127.0.0.1:8411", ""},
		{"127.0.0.1:", "", "127.0.0.1:8411", ""},
		{"127.0.0.1:9000", "", "127.0.0.1:9000", ""},
		{"localhost:9000", "", "localhost:9000", ""},
		{"[::1]:9000", "", "[::1]:9000", ""},
		{"0.0.0.0:8411", testToken, "0.0.0.0:8411", ""},
		{"0.0.0.0:8411", "", "", `--http "0.0.0.0:8411" listens beyond this machine: set PGGLASS_HTTP_TOKEN`},
		{"[::]:8411", "", "", "set PGGLASS_HTTP_TOKEN"},
		{"192.168.1.20:8411", "", "", "set PGGLASS_HTTP_TOKEN"},
		{"db-tools.internal:8411", "", "", "set PGGLASS_HTTP_TOKEN"},
		{"8411", "", "", `--http "8411": want [host]:port, such as :8411 or 127.0.0.1:8411`},
	} {
		got, err := httpAddress(tc.flag, tc.token)
		if got != tc.want || (err == nil) != (tc.err == "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("httpAddress(%q, token set %v) = %q, %v; want %q, %q", tc.flag, tc.token != "", got, err, tc.want, tc.err)
		}
	}
}

func TestLoopbackOrigin(t *testing.T) {
	for origin, want := range map[string]bool{
		"http://localhost:6274":         true,
		"http://127.0.0.1":              true,
		"http://127.0.0.2:8411":         true,
		"https://[::1]:8411":            true,
		"http://evil.example":           false,
		"http://localhost.evil.example": false,
		"http://169.254.169.254":        false,
		"ftp://localhost":               false,
		"null":                          false,
		"file:///etc/passwd":            false,
		"http://[::1":                   false,
	} {
		if got := loopbackOrigin(origin); got != want {
			t.Errorf("loopbackOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

type httpServer struct {
	url  string
	logs *lockedBuffer
	stop func() int
}

func startHTTP(t *testing.T, configPath string) httpServer {
	t.Helper()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", configPath, "--http", "127.0.0.1:0"}, strings.NewReader(""), io.Discard, logs)
	}()
	var once sync.Once
	code := -1
	stop := func() int {
		once.Do(func() {
			cancel()
			code = exitCode(t, done, logs)
		})
		return code
	}
	t.Cleanup(func() {
		if code := stop(); code != 0 {
			t.Errorf("run = %d after shutdown, want 0\nstderr:\n%s", code, logs)
		}
	})
	url := awaitURL(logs)
	if url == "" {
		t.Fatalf("not listening after 10s\nstderr:\n%s", logs)
	}
	return httpServer{url: url, logs: logs, stop: stop}
}

func awaitURL(logs *lockedBuffer) string {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if url := listeningURL(logs.String()); url != "" {
			return url
		}
	}
	return ""
}

func listeningURL(logs string) string {
	for line := range strings.Lines(logs) {
		var entry struct{ Msg, URL string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "listening" {
			return entry.URL
		}
	}
	return ""
}

type withHeader http.Header

func (h withHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for key, values := range h {
		req.Header[key] = values
	}
	return http.DefaultTransport.RoundTrip(req)
}

func connectHTTP(t *testing.T, url string, header http.Header) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: withHeader(header)}}
	session, err := client.Connect(t.Context(), transport, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func ping(t *testing.T, url string, edit func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	edit(req)
	res, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

func setHeader(key, value string) func(*http.Request) {
	return func(req *http.Request) { req.Header.Set(key, value) }
}

func listConnections(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_list_connections"})
	if err != nil {
		t.Fatal(err)
	}
	return textOf(t, res)
}

const unusedDSN = "postgres://127.0.0.1:1/none?sslmode=disable"

// Over Streamable HTTP, a client gets the same tools as over stdio, on the
// newest protocol revision, each call is audited on stderr, and a call in
// flight when the server is told to stop still gets its result.
func TestHTTPServesTheTools(t *testing.T) {
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	isolateHome(t)
	t.Setenv(authEnv, "")
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{
		"app": map[string]any{"source": "direct", "dsn": dsn, "default_schema": "app"},
	})))
	if !strings.HasPrefix(srv.url, "http://127.0.0.1:") || !strings.HasSuffix(srv.url, "/mcp") {
		t.Errorf("url = %s, want http://127.0.0.1:<port>/mcp", srv.url)
	}
	session, err := connectHTTP(t, srv.url, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr:\n%s", err, srv.logs)
	}
	if got, want := session.InitializeResult().ProtocolVersion, mcp.SupportedProtocolVersions()[0]; got != want {
		t.Errorf("protocol version %s, want the newest, %s", got, want)
	}
	checkTools(t, session)
	if got := query(session, "SELECT count(*) FROM customers"); got != "count\n50\n(1 row)" {
		t.Errorf("pg_query = %q", got)
	}
	slow := make(chan string, 1)
	go func() { slow <- query(session, "SELECT 1 AS slept FROM pg_sleep(1)") }()
	awaitActiveQuery(t, "pg_sleep(1)", true)
	if code := srv.stop(); code != 0 {
		t.Errorf("run = %d after shutdown, want 0", code)
	}
	if got := <-slow; got != "slept\n1\n(1 row)" {
		t.Errorf("pg_query in flight at shutdown = %q, want it to finish", got)
	}
	if !strings.Contains(srv.logs.String(), `"tool":"pg_query","connection":"app"`) || strings.Contains(srv.logs.String(), dsn) {
		t.Errorf("stderr lacks the audit line or holds the DSN:\n%s", srv.logs)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := http.DefaultClient.Do(req); err == nil { //nolint:gosec
		_ = res.Body.Close()
		t.Error("still serving after shutdown")
	}
}

func query(session *mcp.ClientSession, sql string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{
		"connection": "app", "sql": sql,
	}})
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	if res.IsError {
		b.WriteString("error: ")
	}
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func awaitActiveQuery(t *testing.T, fragment string, active bool) {
	t.Helper()
	conn := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if testdb.Value[int64](t, conn, "SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND strpos(query, $1) > 0 AND pid <> pg_backend_pid()", fragment) > 0 == active {
			return
		}
	}
	t.Fatalf("after 10s, an active query holding %q is still %t, want %t", fragment, !active, active)
}

// With PGGLASS_HTTP_TOKEN set, a request without that bearer token is
// refused with 401, and the token is never logged.
func TestHTTPRequiresTheBearerToken(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, testToken)
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{"db": direct(unusedDSN)})))
	for name, header := range map[string]http.Header{
		"no token":    nil,
		"wrong token": {"Authorization": {"Bearer " + testToken[1:]}},
		"basic auth":  {"Authorization": {"Basic " + testToken}},
	} {
		if _, err := connectHTTP(t, srv.url, header); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
			t.Errorf("%s: connect error = %v, want Unauthorized", name, err)
		}
		edit := func(req *http.Request) {
			for key, values := range header {
				req.Header[key] = values
			}
		}
		if status := ping(t, srv.url, edit); status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, status)
		}
	}
	session, err := connectHTTP(t, srv.url, http.Header{"Authorization": {"Bearer " + testToken}})
	if err != nil {
		t.Fatalf("connect with the token: %v", err)
	}
	if got := listConnections(t, session); !strings.HasPrefix(got, "Connections (1):\n  db [direct]") {
		t.Errorf("pg_list_connections = %q", got)
	}
	if status := ping(t, strings.TrimSuffix(srv.url, "/mcp")+"/", setHeader("Authorization", "Bearer "+testToken)); status != http.StatusNotFound {
		t.Errorf("POST / status %d, want 404", status)
	}
	if !strings.Contains(srv.logs.String(), `"token_required":true`) || strings.Contains(srv.logs.String(), testToken) {
		t.Errorf("stderr does not say a token is required, or holds the token:\n%s", srv.logs)
	}
}

// A browser page on another origin, or one that rebinds its own name to
// 127.0.0.1, must not reach the server: requests with a foreign Origin or
// Host are refused with 403.
func TestHTTPRefusesForeignOriginsAndHosts(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{})))
	if _, err := connectHTTP(t, srv.url, http.Header{"Origin": {"http://evil.example"}}); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("foreign origin: connect error = %v, want Forbidden", err)
	}
	for name, tc := range map[string]struct {
		edit func(*http.Request)
		want int
	}{
		"no origin":       {func(*http.Request) {}, http.StatusOK},
		"loopback origin": {setHeader("Origin", "http://localhost:6274"), http.StatusOK},
		"foreign origin":  {setHeader("Origin", "http://evil.example"), http.StatusForbidden},
		"null origin":     {setHeader("Origin", "null"), http.StatusForbidden},
		"foreign host":    {func(req *http.Request) { req.Host = "evil.example:8411" }, http.StatusForbidden},
	} {
		if status := ping(t, srv.url, tc.edit); status != tc.want {
			t.Errorf("%s: status %d, want %d", name, status, tc.want)
		}
	}
	session, err := connectHTTP(t, srv.url, http.Header{"Origin": {"http://127.0.0.1:6274"}})
	if err != nil {
		t.Fatalf("loopback origin: connect: %v", err)
	}
	if got := listConnections(t, session); got != "Connections (0):" {
		t.Errorf("pg_list_connections = %q", got)
	}
}

// A client that listens for list changes keeps a stream open; shutting down
// used to wait out the grace period on it, then fail.
func TestHTTPShutdownEndsListenStreams(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{})))
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {},
	})
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if got := listConnections(t, session); got != "Connections (0):" {
		t.Errorf("pg_list_connections = %q", got)
	}
	start := time.Now()
	if code := srv.stop(); code != 0 {
		t.Errorf("run = %d after shutdown, want 0\nstderr:\n%s", code, srv.logs)
	}
	if elapsed := time.Since(start); elapsed > shutdownTimeout/2 {
		t.Errorf("shutdown took %s", elapsed)
	}
}

func TestHTTPRefusesToListenBeyondLoopbackWithoutAToken(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	path := writeConfig(t, connections(map[string]any{}))
	start := time.Now()
	code, _, errOut := runCommand(t, "--config", path, "--http", "0.0.0.0:0")
	if code != 1 || !strings.Contains(errOut, "set PGGLASS_HTTP_TOKEN") || strings.Contains(errOut, `"listening"`) {
		t.Errorf("run = %d, %q; want 1 and a request for PGGLASS_HTTP_TOKEN, without listening", code, errOut)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("refusing took %s", elapsed)
	}
}

func TestHTTPReportsAnAddressItCannotListenOn(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{})))
	busy := strings.TrimSuffix(strings.TrimPrefix(srv.url, "http://"), "/mcp")
	code, _, errOut := runCommand(t, "--config", writeConfig(t, connections(map[string]any{})), "--http", busy)
	if code != 1 || !strings.Contains(errOut, "address already in use") {
		t.Errorf("run on a busy port = %d, %q; want 1 and the listen error", code, errOut)
	}
}
