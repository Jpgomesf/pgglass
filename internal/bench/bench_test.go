package bench

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/spill"
	"github.com/jpgomesf/pgglass/internal/testdb"
	"github.com/jpgomesf/pgglass/internal/tools"
)

var update = flag.Bool("update", false, "rewrite "+baselinePath+" and the table in "+docsPath+" with the sizes measured")

const (
	prod             = "prod-app"
	staging          = "staging-app"
	stagingDB        = "bench_staging"
	shownSpillDir    = "/home/me/.cache/pgglass"
	stagingMigration = `
ALTER TABLE app.orders ADD COLUMN coupon_code text;
CREATE INDEX orders_status_idx ON app.orders (status);
ALTER TABLE app.customers ALTER COLUMN name DROP NOT NULL;
CREATE TABLE app.coupons (code text PRIMARY KEY, percent_off int NOT NULL CHECK (percent_off BETWEEN 1 AND 100));
GRANT SELECT ON app.coupons TO ro_clean;
`
)

var spillFile = regexp.MustCompile(`(spill-[A-Za-z0-9_-]*-[0-9]{8}T[0-9]{6}Z-)[0-9]+(\.csv)`)

type scenario struct {
	name string
	tool string
	args map[string]any
}

var questions = []scenario{
	{"list connections", "pg_list_connections", nil},
	{"find a table", "pg_search", map[string]any{"connection": prod, "pattern": "order"}},
	{"describe a table", "pg_schema", map[string]any{"connection": prod, "table": "orders"}},
	{"filtered query", "pg_query", map[string]any{
		"connection": prod,
		"sql":        "SELECT id, status, total, placed_at FROM orders WHERE status = $1 ORDER BY placed_at DESC LIMIT 10",
		"params":     []any{"paid"},
	}},
	{"large query, spilled", "pg_query", map[string]any{"connection": prod, "sql": "SELECT * FROM events ORDER BY id"}},
	{"profile a column", "pg_profile", map[string]any{"connection": prod, "table": "orders", "column": "status"}},
	{"explain a query", "pg_explain", map[string]any{
		"connection": prod,
		"sql":        "SELECT kind, count(*) FROM events WHERE occurred_at >= '2024-01-10' GROUP BY kind",
	}},
	{"server health", "pg_health", map[string]any{"connection": prod}},
	{"diff two databases", "pg_diff_schema", map[string]any{"left": prod, "right": staging, "schema": "app"}},
	{"fan-out", "pg_query", map[string]any{
		"connections": []string{prod, staging},
		"sql":         "SELECT count(*) AS orders, sum(total) AS revenue FROM orders",
	}},
}

func TestMain(m *testing.M) { testdb.Main(m) }

// TestTokenBudget measures every scenario and fails when one grows more than
// 10% past the baseline, or tools/list passes its cap. With -update, it first
// rewrites the baseline and the table in docs/benchmark.md.
func TestTokenBudget(t *testing.T) {
	got := measure(t)
	t.Logf("sizes on %s:\n%s", testdb.Image(), table(got))
	if *update {
		save(t, got)
	}
	for _, problem := range regressions(load(t), got) {
		t.Error(problem)
	}
	if t.Failed() {
		t.Log("if the growth is intended, run make bench-baseline and commit the files it rewrites")
	}
}

func measure(t *testing.T) []measurement {
	t.Helper()
	dir := t.TempDir()
	session := connect(t, dir)
	list, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	definitions, err := json.Marshal(list.Tools)
	if err != nil {
		t.Fatal(err)
	}
	got := []measurement{
		newMeasurement(toolsList, len(definitions)),
		newMeasurement(instructions, len(session.InitializeResult().Instructions)),
	}
	for _, q := range questions {
		text := call(t, session, q)
		if len(text) > config.InlineMaxBytes {
			t.Errorf("%s: %d bytes, over the inline limit of %d", q.name, len(text), config.InlineMaxBytes)
		}
		got = append(got, newMeasurement(q.name, len(shown(text, dir))))
	}
	return got
}

func connect(t *testing.T, spillDir string) *mcp.ClientSession {
	t.Helper()
	stopAutovacuum(t)
	testdb.CreateDatabase(t, stagingDB, testdb.AppSQL()+stagingMigration)
	cfg := config.Config{Defaults: config.Defaults{}.WithDefaults(), Connections: map[string]config.Connection{
		prod:    appConnection(t, testdb.DBApp),
		staging: appConnection(t, stagingDB),
	}}
	eng := engine.New(&cfg)
	t.Cleanup(eng.Close)
	store := &spill.Store{Dir: spillDir, TTL: time.Hour, InlineMaxBytes: config.InlineMaxBytes}
	server := tools.NewServer(tools.New(cfg, eng, store, io.Discard), "bench")

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "bench"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func stopAutovacuum(t *testing.T) {
	t.Helper()
	conn := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	defer func() { _ = conn.Close(context.Background()) }()
	for _, sql := range []string{"ALTER SYSTEM SET autovacuum = off", "SELECT pg_reload_conf()"} {
		if _, err := conn.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func appConnection(t *testing.T, database string) config.Connection {
	t.Helper()
	return config.Connection{Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, database), DefaultSchema: "app"}
}

func call(t *testing.T, session *mcp.ClientSession, q scenario) string {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: q.tool, Arguments: q.args})
	if err != nil {
		t.Fatalf("%s: %v", q.name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content items, want 1", q.name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || res.IsError {
		t.Fatalf("%s: want a text result that is not an error, got %#v", q.name, res.Content[0])
	}
	return text.Text
}

func shown(text, spillDir string) string {
	text = strings.ReplaceAll(text, spillDir, shownSpillDir)
	return spillFile.ReplaceAllString(text, "${1}4294967295${2}")
}
