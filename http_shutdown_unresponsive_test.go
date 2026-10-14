package main

import (
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

type stallingProxy struct {
	ln      net.Listener
	target  string
	stalled atomic.Bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

func startStallingProxy(t *testing.T, target string) *stallingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallingProxy{ln: ln, target: target, stop: make(chan struct{})}
	p.wg.Go(p.accept)
	t.Cleanup(func() {
		close(p.stop)
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *stallingProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Go(func() { p.serve(client) })
	}
}

func (p *stallingProxy) serve(client net.Conn) {
	defer func() { _ = client.Close() }()
	if p.stalled.Load() {
		<-p.stop
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go p.forward(server, client)
	p.forward(client, server)
}

func (p *stallingProxy) forward(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if p.stalled.Load() {
			<-p.stop
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

// When the database stops answering, as across a network partition, the
// server still stops about 10 seconds after a signal, once it has cancelled
// the calls still running: it does not wait on Postgres to answer their
// cancellation.
func TestHTTPShutdownDoesNotWaitOnAnUnresponsiveDatabase(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	dsn, err := url.Parse(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))
	if err != nil {
		t.Fatal(err)
	}
	proxy := startStallingProxy(t, dsn.Host)
	dsn.Host = proxy.ln.Addr().String()
	cfg := writeConfig(t, connections(map[string]any{"app": direct(dsn.String())}))
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", cfg, "--http", "127.0.0.1:0"}, strings.NewReader(""), io.Discard, logs)
	}()
	endpoint := awaitURL(logs)
	if endpoint == "" {
		t.Fatalf("not listening after 10s\nstderr:\n%s", logs)
	}
	session, err := connectHTTP(t, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := query(session, "SELECT 1 AS one"); got != "one\n1\n(1 row)" {
		t.Fatalf("pg_query = %q", got)
	}
	go func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{
			"connection": "app", "sql": "SELECT 1 FROM pg_sleep(20) AS cut_off_from_the_server",
		}})
	}()
	awaitActiveQuery(t, "cut_off_from_the_server", true)
	proxy.stalled.Store(true)
	start := time.Now()
	cancel()
	select {
	case code := <-done:
		if took := time.Since(start); code != 1 || took > shutdownTimeout+5*time.Second {
			t.Errorf("run = %d after %s, want 1 after about %s\nstderr:\n%s", code, took, shutdownTimeout, logs)
		}
	case <-time.After(4 * shutdownTimeout):
		t.Fatalf("run still going %s after shutdown began\nstderr:\n%s", time.Since(start), logs)
	}
}
