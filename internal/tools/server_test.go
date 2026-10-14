package tools_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/spill"
	"github.com/jpgomesf/pgglass/internal/testdb"
	"github.com/jpgomesf/pgglass/internal/tools"
)

func TestMain(m *testing.M) {
	guard.Warm()
	testdb.Main(m)
}

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

type session struct {
	*mcp.ClientSession
	audit    *lockedBuffer
	spillDir string
}

func connect(t *testing.T, cfg config.Config) *session {
	t.Helper()
	return connectSpillingTo(t, cfg, t.TempDir())
}

func connectSpillingTo(t *testing.T, cfg config.Config, dir string) *session {
	t.Helper()
	return connectAt(t, cfg, dir, "")
}

func connectAt(t *testing.T, cfg config.Config, dir, protocolVersion string) *session {
	t.Helper()
	return connectClient(t, cfg, dir, protocolVersion, nil)
}

func connectClient(t *testing.T, cfg config.Config, dir, protocolVersion string, opts *mcp.ClientOptions) *session {
	t.Helper()
	eng := engine.New(&cfg)
	t.Cleanup(eng.Close)
	s := &session{audit: &lockedBuffer{}, spillDir: dir}
	store := &spill.Store{Dir: dir, TTL: time.Hour, InlineMaxBytes: config.InlineMaxBytes}
	server := tools.NewServer(tools.New(cfg, eng, store, s.audit), "test")

	serverSide, clientSide := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, opts)
	cs, err := client.Connect(t.Context(), clientSide, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s.ClientSession = cs
	return s
}

func newConfig(roleCheck string, conns map[string]config.Connection) config.Config {
	return config.Config{Defaults: config.Defaults{RoleCheck: roleCheck}.WithDefaults(), Connections: conns}
}

func direct(dsn string) config.Connection {
	return config.Connection{Source: "direct", DSN: dsn}
}

const unusedDSN = "postgres://127.0.0.1:1/none?sslmode=disable"

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

func (s *session) ok(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	text, isErr := s.call(t, tool, args)
	if isErr {
		t.Fatalf("%s %v: error result:\n%s", tool, args, text)
	}
	return text
}

func (s *session) fails(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	text, isErr := s.call(t, tool, args)
	if !isErr {
		t.Fatalf("%s %v: IsError unset, want an error result; got:\n%s", tool, args, text)
	}
	return text
}

func sha12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

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

func (s *session) auditedCall(t *testing.T, tool string, a map[string]any) (string, bool, []auditLine) {
	t.Helper()
	before := len(s.auditLines(t))
	text, isErr := s.call(t, tool, a)
	lines := s.auditLines(t)[before:]
	for i := range lines {
		if lines[i].DurationMS == nil || *lines[i].DurationMS < 0 {
			t.Errorf("audit line %+v: duration_ms = %v", lines[i], lines[i].DurationMS)
		}
		lines[i].DurationMS = nil
	}
	slices.SortFunc(lines, func(a, b auditLine) int { return strings.Compare(a.Connection, b.Connection) })
	return text, isErr, lines
}

func listedTool(t *testing.T, name string) (*mcp.Tool, int) {
	t.Helper()
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	res, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tool *mcp.Tool) bool { return tool.Name == name })
	if i < 0 {
		t.Fatalf("%s is not listed among %d tools", name, len(res.Tools))
	}
	def, err := json.Marshal(res.Tools[i])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s definition: %d bytes of JSON", name, len(def))
	return res.Tools[i], len(def)
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
	if got := strings.Join(names, " "); got != "pg_diff_schema pg_explain pg_health pg_list_connections pg_profile pg_query pg_schema pg_search" {
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

func TestEveryToolHasAHumanReadableTitleOfItsOwn(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	res, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, tool := range res.Tools {
		if tool.Title == "" || strings.Contains(tool.Title, "_") || titles[tool.Title] {
			t.Errorf("%s title = %q, want a human-readable title no other tool has", tool.Name, tool.Title)
		}
		titles[tool.Title] = true
	}
}

func TestServerIdentifiesAsPgglass(t *testing.T) {
	want := mcp.Implementation{
		Name:        "pgglass",
		Title:       "pgglass",
		Description: "Read-only PostgreSQL for AI agents across many databases, enforced by the server",
		Version:     "test",
		WebsiteURL:  "https://github.com/Jpgomesf/pgglass",
	}
	for _, version := range []string{mcp.SupportedProtocolVersions()[0], "2025-11-25"} {
		s := connectAt(t, newConfig(config.RoleCheckWarn, nil), t.TempDir(), version)
		if info := s.InitializeResult().ServerInfo; info == nil || !reflect.DeepEqual(*info, want) {
			t.Errorf("%s: server info = %+v, want %+v", version, info, want)
		}
	}
}

func TestInstructionsTellAgentsHowToUseTheTools(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	got := s.InitializeResult().Instructions
	if len(got) >= 2048 {
		t.Errorf("instructions take %d bytes, want fewer than 2048", len(got))
	}
	for _, want := range []string{"pg_list_connections", "pg_schema", "pg_search", "params", "connections", "CSV file"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions do not mention %q:\n%s", want, got)
		}
	}
}

func TestServerNegotiatesTheNewestProtocolVersionAndEveryOlderOne(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, nil)
	instructions := connect(t, cfg).InitializeResult().Instructions
	cases := map[string]string{"": mcp.SupportedProtocolVersions()[0]}
	for _, v := range mcp.SupportedProtocolVersions() {
		cases[v] = v
	}
	for ask, want := range cases {
		s := connectAt(t, cfg, t.TempDir(), ask)
		if res := s.InitializeResult(); res.ProtocolVersion != want || res.Instructions != instructions {
			t.Errorf("client asking for %q: version %q, instructions %q; want version %q and the server's instructions",
				ask, res.ProtocolVersion, res.Instructions, want)
		}
		if got := s.ok(t, "pg_list_connections", nil); got != "Connections (0):" {
			t.Errorf("client asking for %q: pg_list_connections = %q", ask, got)
		}
	}
}

func TestListConnectionsInNameOrder(t *testing.T) {
	const neverFetched = "pgglass/tools-test/never-fetched"
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"zeta":  direct(unusedDSN),
		"alpha": {Source: "direct", DSN: unusedDSN, DefaultSchema: "app", RequiresVPN: true},
		"mid":   {Source: "env", DSNEnvVar: "PGGLASS_TOOLS_TEST_UNSET"},
		"prod":  {Source: "aws_secrets_manager", SecretName: neverFetched, Confirm: true},
	})
	s := connect(t, cfg)
	want := "Connections (4):\n" +
		"  alpha [direct, vpn, schema:app]\n    role not checked yet: checked on first use\n" +
		"  mid [env]\n    role not checked yet: checked on first use\n" +
		"  prod [aws_secrets_manager, confirm, not loaded]\n    role not checked yet: checked on first use\n" +
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
		{"pg_schema", map[string]any{"connection": "db", "tables": "orders"}, `invalid arguments: json: unknown field "tables"`},
		{"pg_search", map[string]any{"connection": "db", "query": "x"}, `invalid arguments: json: unknown field "query"`},
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

// An unknown connection fails each tool before any role check or query: the
// error is the list of configured names alone.
func TestUnknownConnectionFailsEveryToolAtOnce(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"db": direct(unusedDSN)}))
	const want = `unknown connection "nope" (available: db)`
	calls := map[string]map[string]any{
		"pg_schema":      {"connection": "nope"},
		"pg_search":      {"connection": "nope", "pattern": "x"},
		"pg_query":       {"connection": "nope", "sql": "SELECT 1"},
		"pg_explain":     {"connection": "nope", "sql": "SELECT 1"},
		"pg_profile":     {"connection": "nope", "table": "app.orders"},
		"pg_health":      {"connection": "nope"},
		"pg_diff_schema": {"left": "nope", "right": "db"},
	}
	for tool, a := range calls {
		if got := s.fails(t, tool, a); got != want {
			t.Errorf("%s: %q, want %q", tool, got, want)
		}
	}
	lines := s.auditLines(t)
	for _, line := range lines {
		if line.Outcome != "error" || line.Connection != "" {
			t.Errorf("audit line %+v, want outcome error and no connection", line)
		}
	}
	if len(lines) != len(calls) {
		t.Errorf("%d audit lines, want %d", len(lines), len(calls))
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
