package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// The built binary, configured by a config.json next to it, serves the tools
// over stdio, logs to stderr only, and removes stale spill files on start.
func TestStdioServer(t *testing.T) {
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	bin := buildServer(t, map[string]any{
		"defaults":    map[string]any{"readonly": true},
		"connections": map[string]any{"app": map[string]any{"source": "direct", "dsn": dsn, "default_schema": "app"}},
	})
	// The server spills under the user cache directory, which HOME (macOS)
	// or XDG_CACHE_HOME (Linux) locates. Set after the build, which caches
	// there too.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	stale := plantStaleSpillFile(t, filepath.Join(cacheDir, "mcp-postgres"))

	cmd := exec.Command(bin) //nolint:gosec // the binary this test just built
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr.Close() }()
	cmd.Stderr = stderr // the server writes to the file itself, so it can be read at any time
	logs := func() string {
		data, _ := os.ReadFile(stderr.Name()) //nolint:gosec // the file this test created
		return string(data)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr:\n%s", err, logs())
	}
	checkTools(t, session)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: map[string]any{
		"connection": "app", "sql": "SELECT count(*) FROM customers",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := res.Content[0].(*mcp.TextContent); !ok || res.IsError || text.Text != "count\n50\n(1 row)" {
		t.Errorf("pg_query = %+v", res.Content[0])
	}
	if err := session.Close(); err != nil {
		t.Errorf("close: %v\nstderr:\n%s", err, logs())
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spill file still there (%v)", err)
	}
	checkStderr(t, logs(), dsn)
}

// buildServer builds the server into a new directory, next to a config.json
// holding cfg, and returns the binary's path.
func buildServer(t *testing.T, cfg map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-postgres")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

// plantStaleSpillFile writes a two-day-old spill file in dir and returns its path.
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
	if got := strings.Join(names, " "); got != "pg_list_connections pg_query pg_schema pg_search" {
		t.Errorf("tools = %s", got)
	}
}

// checkStderr checks that every line the server logged is JSON, that they
// include the readonly deprecation warning and the pg_query audit line, and
// that none holds the DSN.
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
