//go:build unix

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A call stuck reading its connection's credentials, here from a dotenv file
// that is a named pipe nothing writes to, does not keep the server from
// stopping: once shutdown's 10 seconds run out, it exits with code 1, as it
// does with any other call that outlasts them.
func TestHTTPShutdownDoesNotWaitOnCredentialsThatNeverArrive(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	fifo := filepath.Join(t.TempDir(), "creds.env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, connections(map[string]any{
		"app": map[string]any{"source": "dotenv", "dotenv_path": fifo, "dsn_env_var": "DATABASE_URL"},
	}))
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", cfg, "--http", "127.0.0.1:0"}, strings.NewReader(""), io.Discard, logs)
	}()
	url := awaitURL(logs)
	if url == "" {
		t.Fatalf("not listening after 10s\nstderr:\n%s", logs)
	}
	session, err := connectHTTP(t, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{"connection": "app", "sql": "SELECT 1"}})
	}()
	writer := awaitReader(t, fifo)
	defer func() { _ = writer.Close() }()
	start := time.Now()
	cancel()
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("run = %d after %s, want 1\nstderr:\n%s", code, time.Since(start), logs)
		}
	case <-time.After(shutdownTimeout + 5*time.Second):
		t.Errorf("run still going %s after shutdown began, with a call stuck reading its credentials\nstderr:\n%s", time.Since(start), logs)
		_ = writer.Close()
		<-done
	}
}

func awaitReader(t *testing.T, fifo string) *os.File {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return w
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
	}
	t.Fatal("the call never opened the dotenv file")
	return nil
}
