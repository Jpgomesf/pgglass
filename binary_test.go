//go:build unix

package main

import (
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The built binary prints the version goreleaser links into it, stops
// cleanly, with exit code 0, on SIGINT and SIGTERM, and stops at once on a
// second signal while it waits for requests to finish.
func TestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := filepath.Join(t.TempDir(), "pgglass")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=9.8.7", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").Output() //nolint:gosec
	if err != nil || string(out) != "pgglass 9.8.7\n" {
		t.Errorf("--version = %q, %v; want pgglass 9.8.7", out, err)
	}

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) { stopsOn(t, bin, sig) })
	}
	t.Run("second interrupt", func(t *testing.T) { stopsAtOnceOnASecondInterrupt(t, bin) })
}

func stopsOn(t *testing.T, bin string, sig syscall.Signal) {
	b := startBinary(t, bin)
	session, err := connectHTTP(t, b.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := listConnections(t, session); got != "Connections (0):" {
		t.Errorf("pg_list_connections = %q", got)
	}
	if err := b.signal(sig); err != nil {
		t.Errorf("after %s: %v, want exit code 0\nstderr:\n%s", sig, err, b.logs)
	}
}

func stopsAtOnceOnASecondInterrupt(t *testing.T, bin string) {
	b := startBinary(t, bin)
	u, err := url.Parse(b.url)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pending.Close() }()
	if _, err := pending.Write([]byte("POST /mcp HTTP/1.1\r\nHost: " + u.Host + "\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := b.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			break
		}
		_ = conn.Close()
	}
	time.Sleep(100 * time.Millisecond)
	var exit *exec.ExitError
	if err := b.signal(syscall.SIGINT); !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
		t.Errorf("after a second SIGINT: %v, want death by SIGINT\nstderr:\n%s", err, b.logs)
	}
}

type binary struct {
	cmd    *exec.Cmd
	url    string
	logs   *lockedBuffer
	exited chan error
}

func startBinary(t *testing.T, bin string) binary {
	t.Helper()
	home := t.TempDir()
	b := binary{logs: &lockedBuffer{}, exited: make(chan error, 1)}
	b.cmd = exec.Command(bin, "--config", writeConfig(t, connections(map[string]any{})), "--http", "127.0.0.1:0") //nolint:gosec
	b.cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+filepath.Join(home, "cache"), authEnv+"=")
	b.cmd.Stderr = b.logs
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { b.exited <- b.cmd.Wait() }()
	t.Cleanup(func() { _ = b.cmd.Process.Kill() })
	if b.url = awaitURL(b.logs); b.url == "" {
		t.Fatalf("not listening after 10s\nstderr:\n%s", b.logs)
	}
	return b
}

func (b binary) signal(sig syscall.Signal) error {
	if err := b.cmd.Process.Signal(sig); err != nil {
		return err
	}
	select {
	case err := <-b.exited:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("still running 15s later")
	}
}
