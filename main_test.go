package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	doc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func connections(conns map[string]any) map[string]any {
	return map[string]any{"connections": conns}
}

func direct(dsn string) map[string]any {
	return map[string]any{"source": "direct", "dsn": dsn}
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func runCommand(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func connectStdio(t *testing.T, args ...string) (*mcp.ClientSession, *lockedBuffer, <-chan int) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	logs := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		code := run(t.Context(), args, inR, outW, logs)
		_ = outW.Close()
		done <- code
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr:\n%s", err, logs)
	}
	return session, logs, done
}

func exitCode(t *testing.T, done <-chan int, logs *lockedBuffer) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(2 * shutdownTimeout):
		t.Fatalf("run did not return within %s\nstderr:\n%s", 2*shutdownTimeout, logs)
		return -1
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("%d content items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return text.Text
}

func TestVersionPrintsTheReleaseVersionOrDev(t *testing.T) {
	defer func(v string) { version = v }(version)
	for set, want := range map[string]string{"1.4.2": "pgglass 1.4.2\n", "": "pgglass dev\n"} {
		version = set
		if code, out, errOut := runCommand(t, "--version"); code != 0 || out != want || errOut != "" {
			t.Errorf("version %q: --version = %d, %q, %q; want 0, %q", set, code, out, errOut, want)
		}
	}
}

func TestHelpPrintsTheUsage(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		code, out, errOut := runCommand(t, flag)
		if code != 0 || out != usage || errOut != "" {
			t.Errorf("%s = %d, %q, %q; want 0 and the usage", flag, code, out, errOut)
		}
	}
	for _, want := range []string{"--config FILE", "$XDG_CONFIG_HOME/pgglass/config.json", "~/.config/pgglass/config.json", "--http ADDR", "PGGLASS_HTTP_TOKEN", "doctor", "--version"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}

func TestMisuseExitsWithCode2(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--nope"}, "flag provided but not defined: -nope"},
		{[]string{"--config"}, "flag needs an argument: -config"},
		{[]string{"--http"}, "flag needs an argument: -http"},
		{[]string{"serve"}, `unexpected argument "serve"`},
		{[]string{"doctor", "extra"}, `unexpected argument "extra"`},
		{[]string{"doctor", "--nope"}, "flag provided but not defined: -nope"},
		{[]string{"doctor", "--http", ":8411"}, "doctor serves nothing: drop --http"},
	} {
		code, out, errOut := runCommand(t, tc.args...)
		if code != 2 || out != "" || errOut != "pgglass: "+tc.want+"\n\n"+usage {
			t.Errorf("%v = %d, %q, %q; want 2 and %q with the usage on stderr", tc.args, code, out, errOut, tc.want)
		}
	}
}

func TestServeLogsWhyItCannotStart(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, "")
	missing := filepath.Join(t.TempDir(), "missing.json")
	unknown := writeConfig(t, connections(map[string]any{"db": map[string]any{"source": "vault"}}))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", missing}, "read config: open " + missing},
		{[]string{"--config", unknown}, `connection "db": unknown source "vault"`},
		{[]string{"--config", unknown, "--http", "0.0.0.0:8411"}, "set PGGLASS_HTTP_TOKEN"},
	} {
		code, out, errOut := runCommand(t, tc.args...)
		var line struct {
			Level, Msg, Error string
		}
		if err := json.Unmarshal([]byte(errOut), &line); err != nil || code != 1 || out != "" ||
			line.Level != "ERROR" || line.Msg != "pgglass stopped" || !strings.Contains(line.Error, tc.want) {
			t.Errorf("%v = %d, %q, %q; want 1 and one JSON line on stderr whose error holds %q", tc.args, code, out, errOut, tc.want)
		}
	}
}

func TestConfigDefaultsToTheUserConfigDirectory(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".config", "pgglass", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"defaults": {"readonly": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCommand(t); code != 0 || !strings.Contains(errOut, "readonly is deprecated") {
		t.Errorf("serving until stdin ends = %d, %q; want 0 and the warning the default config file causes", code, errOut)
	}
	if code, out, errOut := runCommand(t, "doctor"); code != 0 || !strings.HasPrefix(out, "config "+path+" ") {
		t.Errorf("doctor = %d, %q, %q; want 0 and the default config file", code, out, errOut)
	}

	t.Setenv("HOME", "")
	for _, args := range [][]string{{"doctor"}, nil} {
		if code, _, errOut := runCommand(t, args...); code != 1 || !strings.Contains(errOut, "no config file: pass --config FILE") {
			t.Errorf("%v without HOME = %d, %q; want 1 and a request for --config", args, code, errOut)
		}
	}
}

// XDG_CONFIG_HOME, when it is an absolute path, moves the default config file
// to $XDG_CONFIG_HOME/pgglass/config.json; a relative one is ignored, as the
// XDG Base Directory Specification asks.
func TestConfigDefaultsToXDGConfigHome(t *testing.T) {
	home := isolateHome(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	path := filepath.Join(xdg, "pgglass", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if code, out, errOut := runCommand(t, "doctor"); code != 0 || !strings.HasPrefix(out, "config "+path+" ") {
		t.Errorf("doctor with XDG_CONFIG_HOME = %d, %q, %q; want 0 and %s", code, out, errOut, path)
	}
	t.Setenv("HOME", "")
	if code, out, errOut := runCommand(t, "doctor"); code != 0 || !strings.HasPrefix(out, "config "+path+" ") {
		t.Errorf("doctor with XDG_CONFIG_HOME and without HOME = %d, %q, %q; want 0 and %s", code, out, errOut, path)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative")
	fallback := filepath.Join(home, ".config", "pgglass", "config.json")
	if code, _, errOut := runCommand(t, "doctor"); code != 1 || !strings.Contains(errOut, "read config: open "+fallback) {
		t.Errorf("doctor with a relative XDG_CONFIG_HOME = %d, %q; want 1 and %s", code, errOut, fallback)
	}
}

// The server serves the tools over stdio, logs to stderr only, removes stale
// spill files on start and stops when the client disconnects.
func TestStdioServesTheTools(t *testing.T) {
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	path := writeConfig(t, map[string]any{
		"defaults":    map[string]any{"readonly": true},
		"connections": map[string]any{"app": map[string]any{"source": "direct", "dsn": dsn, "default_schema": "app"}},
	})
	isolateHome(t)
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	stale := plantStaleSpillFile(t, filepath.Join(cacheDir, "pgglass"))

	session, logs, done := connectStdio(t, "--config", path)
	info := session.InitializeResult()
	if info.ProtocolVersion != mcp.SupportedProtocolVersions()[0] || info.ServerInfo.Version != "dev" {
		t.Errorf("protocol version %s, server version %s; want the newest, %s, and dev",
			info.ProtocolVersion, info.ServerInfo.Version, mcp.SupportedProtocolVersions()[0])
	}
	checkTools(t, session)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{
		"connection": "app", "sql": "SELECT count(*) FROM customers",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if text := textOf(t, res); res.IsError || text != "count\n50\n(1 row)" {
		t.Errorf("pg_query = %q", text)
	}
	if err := session.Close(); err != nil {
		t.Errorf("close: %v\nstderr:\n%s", err, logs)
	}
	if code := exitCode(t, done, logs); code != 0 {
		t.Errorf("run = %d after the client left, want 0\nstderr:\n%s", code, logs)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spill file still there (%v)", err)
	}
	checkStderr(t, logs.String(), dsn)
}

// A spill directory the server cannot clean up is logged, and the server
// serves anyway.
func TestServeWarnsWhenSpillCleanupFails(t *testing.T) {
	isolateHome(t)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "pgglass"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCommand(t, "--config", writeConfig(t, connections(map[string]any{})))
	var line struct{ Level, Msg, Error string }
	if err := json.Unmarshal([]byte(errOut), &line); err != nil || code != 0 || out != "" ||
		line.Level != "WARN" || line.Msg != "spill cleanup failed" || !strings.Contains(line.Error, filepath.Join(cache, "pgglass")) {
		t.Errorf("serving until stdin ends = %d, %q, %q; want 0 and one warning naming the spill directory", code, out, errOut)
	}
}

func TestStdioStopsWhenTheContextEnds(t *testing.T) {
	isolateHome(t)
	path := writeConfig(t, connections(map[string]any{}))
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	logs := &lockedBuffer{}
	go func() { done <- run(ctx, []string{"--config", path}, inR, io.Discard, logs) }()
	cancel()
	if code := exitCode(t, done, logs); code != 0 || logs.String() != "" {
		t.Errorf("run = %d, stderr %q after the context ended; want 0 and no log", code, logs)
	}
}

func plantStaleSpillFile(t *testing.T, dir string) string {
	t.Helper()
	stale := filepath.Join(dir, "spill-pg_query-app-20200101T000000Z-123456.csv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("a\r\n1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	return stale
}

func checkTools(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, " "); got != "pg_diff_schema pg_explain pg_health pg_list_connections pg_profile pg_query pg_schema pg_search" {
		t.Errorf("tools = %s", got)
	}
}

func checkStderr(t *testing.T, logs, dsn string) {
	t.Helper()
	if strings.Contains(logs, dsn) {
		t.Errorf("stderr holds the DSN:\n%s", logs)
	}
	var deprecation, audit bool
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Errorf("stderr line is not JSON: %q", scanner.Text())
			continue
		}
		msg, _ := line["msg"].(string)
		deprecation = deprecation || (line["level"] == "WARN" && strings.Contains(msg, "readonly is deprecated"))
		audit = audit || (line["tool"] == "pg_query" && line["outcome"] == "ok" && line["connection"] == "app")
	}
	if !deprecation || !audit {
		t.Errorf("stderr lacks the deprecation warning (%v) or the pg_query audit line (%v):\n%s", deprecation, audit, logs)
	}
}
