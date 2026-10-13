package tools_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
	"github.com/jpgomesf/mcp-postgres/internal/tools"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// lockedBuffer collects the audit log, which calls running in parallel write.
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

// session is a client connected in memory to a server built on a config.
type session struct {
	*mcp.ClientSession
	audit    *lockedBuffer
	spillDir string
}

func connect(t *testing.T, cfg config.Config) *session {
	t.Helper()
	eng := engine.New(&cfg)
	t.Cleanup(eng.Close)
	s := &session{audit: &lockedBuffer{}, spillDir: t.TempDir()}
	store := &spill.Store{Dir: s.spillDir, TTL: time.Hour, InlineMaxBytes: config.InlineMaxBytes}
	server := tools.NewServer(tools.New(cfg, eng, store, s.audit), "test")

	serverSide, clientSide := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s.ClientSession = cs
	return s
}

// newConfig returns a config with the given role check and connections, and
// every other setting at its default.
func newConfig(roleCheck string, conns map[string]config.Connection) config.Config {
	return config.Config{Defaults: config.Defaults{RoleCheck: roleCheck}.WithDefaults(), Connections: conns}
}

// direct is a connection with a fixed DSN.
func direct(dsn string) config.Connection {
	return config.Connection{Source: "direct", DSN: dsn}
}

// unusedDSN is never dialed: the tests that use it fail before any query.
const unusedDSN = "postgres://127.0.0.1:1/none?sslmode=disable"

// call calls tool and returns the text of its result and whether it is an
// error. A protocol error fails the test: every failure must be a result.
func (s *session) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	return resultText(t, res), res.IsError
}

func resultText(t testing.TB, res *mcp.CallToolResult) string {
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

// ok calls tool and fails the test if the result is an error.
func (s *session) ok(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	text, isErr := s.call(t, tool, args)
	if isErr {
		t.Fatalf("%s %v: error result:\n%s", tool, args, text)
	}
	return text
}

// fails calls tool and fails the test unless the result is an error.
func (s *session) fails(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	text, isErr := s.call(t, tool, args)
	if !isErr {
		t.Fatalf("%s %v: IsError unset, want an error result; got:\n%s", tool, args, text)
	}
	return text
}

// sha12 is the first 12 hex digits of the SHA-256 of s.
func sha12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// auditLine is one line of the audit log.
type auditLine struct {
	Msg        string `json:"msg"`
	Tool       string `json:"tool"`
	Connection string `json:"connection"`
	SQLSHA256  string `json:"sql_sha256"`
	SQLLen     int    `json:"sql_len"`
	DurationMS *int64 `json:"duration_ms"`
	Rows       int    `json:"rows"`
	Truncated  bool   `json:"truncated"`
	Outcome    string `json:"outcome"`
	SQLState   string `json:"sqlstate"`
}

// auditLines decodes the audit log, failing the test on a line that is not
// JSON or that holds keys an audit line must not have.
func (s *session) auditLines(t *testing.T) []auditLine {
	t.Helper()
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "tool": true, "connection": true,
		"sql_sha256": true, "sql_len": true, "duration_ms": true, "rows": true, "truncated": true,
		"outcome": true, "sqlstate": true}
	var lines []auditLine
	scanner := bufio.NewScanner(strings.NewReader(s.audit.String()))
	for scanner.Scan() {
		var keys map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &keys); err != nil {
			t.Fatalf("audit line %q is not JSON: %v", scanner.Text(), err)
		}
		if len(keys) != len(allowed) {
			t.Errorf("audit line %s has %d keys, want %d", scanner.Text(), len(keys), len(allowed))
		}
		for k := range keys {
			if !allowed[k] {
				t.Errorf("audit line has unexpected key %q: %s", k, scanner.Text())
			}
		}
		var line auditLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestToolsListIsAnnotatedAndSmall(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	res, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint ||
			a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want read-only, idempotent, not destructive, closed world", tool.Name, a)
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("%s has no description or input schema", tool.Name)
		}
	}
	if got := strings.Join(names, " "); got != "pg_diff_schema pg_explain pg_list_connections pg_profile pg_query pg_schema pg_search" {
		t.Errorf("tools = %s", got)
	}
	defs, err := json.Marshal(res.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) > 6000 {
		t.Errorf("tool definitions take %d bytes of JSON, want at most 6000", len(defs))
	}
	t.Logf("tool definitions: %d bytes of JSON", len(defs))
}

func TestListConnectionsInNameOrder(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"zeta":  direct(unusedDSN),
		"alpha": {Source: "direct", DSN: unusedDSN, DefaultSchema: "app", RequiresVPN: true},
		"mid":   {Source: "env", DSNEnvVar: "MCP_PG_TOOLS_TEST_UNSET"},
	})
	s := connect(t, cfg)
	want := "Connections (3):\n" +
		"  alpha [direct, vpn, schema:app]\n    role not checked yet: checked on first use\n" +
		"  mid [env]\n    role not checked yet: checked on first use\n" +
		"  zeta [direct]\n    role not checked yet: checked on first use"
	for range 3 {
		if got := s.ok(t, "pg_list_connections", nil); got != want {
			t.Fatalf("pg_list_connections =\n%s\nwant\n%s", got, want)
		}
	}

	off := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"a": direct(unusedDSN)}))
	if got := off.ok(t, "pg_list_connections", map[string]any{}); got != "Connections (1):\n  a [direct]\n    role check off" {
		t.Errorf("role_check off: %s", got)
	}
}

// Every failure is a result with IsError set and a reason, never a protocol
// error. These fail before any query, so they need no database.
func TestInvalidCallsAreToolErrors(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"db":  direct(unusedDSN),
		"app": {Source: "direct", DSN: unusedDSN, DefaultSchema: "app"},
	})
	s := connect(t, cfg)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pg_query", map[string]any{"connection": "nope", "sql": "SELECT 1"}, `unknown connection "nope" (available: app, db)`},
		{"pg_query", map[string]any{"sql": "SELECT 1"}, `unknown connection ""`},
		{"pg_query", map[string]any{"connection": "db"}, "query rejected: SQL must be exactly one statement, got 0"},
		{"pg_query", map[string]any{"connection": "db", "sql": "DELETE FROM app.orders"}, "query rejected: DeleteStmt is not allowed"},
		{"pg_query", map[string]any{"connection": "db", "sql": "SELECT 1", "format": "csv"}, "format must be one of tsv, json, markdown"},
		{"pg_query", map[string]any{"connection": "db", "sql": "SELECT 1", "max_rows": -1}, "max_rows must be at least 1"},
		{"pg_query", map[string]any{"connection": "db", "sql": "SELECT 1", "max_rows": 2.5}, "invalid arguments"},
		{"pg_query", map[string]any{"connection": "db", "query": "SELECT 1"}, `invalid arguments: json: unknown field "query"`},
		{"pg_query", map[string]any{"connection": "db", "sql": 1}, "invalid arguments"},
		{"pg_schema", map[string]any{"connection": "nope"}, `unknown connection "nope"`},
		{"pg_schema", map[string]any{"connection": "db", "table": "orders"}, "table needs schema"},
		{"pg_search", map[string]any{"connection": "db"}, "pattern must not be empty"},
		{"pg_search", map[string]any{"connection": "db", "pattern": "x", "limit": -3}, "limit must be at least 1"},
		{"pg_list_connections", map[string]any{"verbose": true}, `unknown field "verbose"`},
	} {
		if got := s.fails(t, tc.tool, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s %v = %q, want it to contain %q", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestAuditLinesForCallsThatFailEarly(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"db": direct(unusedDSN)}))
	const markedSQL = "DELETE FROM app.orders WHERE note = 'zebra-marker-42'"
	s.fails(t, "pg_query", map[string]any{"connection": "db", "sql": markedSQL})
	s.fails(t, "pg_query", map[string]any{"connection": "Robert'); DROP TABLE students;--", "sql": "SELECT 1"})
	s.ok(t, "pg_list_connections", nil)

	lines := s.auditLines(t)
	if len(lines) != 3 {
		t.Fatalf("%d audit lines, want 3:\n%s", len(lines), s.audit)
	}
	want := []auditLine{
		{Msg: "tool call", Tool: "pg_query", Connection: "db", SQLSHA256: sha12(markedSQL), SQLLen: len(markedSQL), Outcome: "guard_reject"},
		// An unknown connection name is the caller's text: it is not logged.
		{Msg: "tool call", Tool: "pg_query", SQLSHA256: sha12("SELECT 1"), SQLLen: 8, Outcome: "error"},
		{Msg: "tool call", Tool: "pg_list_connections", Outcome: "ok"},
	}
	for i, line := range lines {
		if line.DurationMS == nil || *line.DurationMS < 0 {
			t.Errorf("line %d: duration_ms = %v", i, line.DurationMS)
		}
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("line %d = %+v\nwant     %+v", i, line, want[i])
		}
	}
	for _, leak := range []string{"zebra-marker-42", "DELETE", "students"} {
		if strings.Contains(s.audit.String(), leak) {
			t.Errorf("audit log contains %q:\n%s", leak, s.audit)
		}
	}
}
