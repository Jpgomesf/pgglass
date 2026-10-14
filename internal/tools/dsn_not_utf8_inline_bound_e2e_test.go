package tools_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// A failed connection's error names the user and database of its DSN, which
// an env var or a .env file can fill with bytes that are not UTF-8, and JSON
// carries each such byte as U+FFFD, three bytes: the error, the role check's
// note of it, pg_list_connections and a resource read keep to the inline
// limit as the client decodes them.
func TestConnectErrorThatIsNotUTF8StaysWithinTheInlineLimit(t *testing.T) {
	t.Setenv("PGGLASS_TEST_NOT_UTF8_DSN", "host=127.0.0.1 port=1 user="+strings.Repeat("\xff", 4_000)+" dbname=x sslmode=disable connect_timeout=2")
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"db": {Source: "env", DSNEnvVar: "PGGLASS_TEST_NOT_UTF8_DSN"},
	}))
	check := func(what, got string) {
		t.Helper()
		if len(got) > config.InlineMaxBytes || !strings.Contains(got, "connection refused") {
			t.Errorf("%s: %d bytes at the client, %d of them U+FFFD, limit %d:\n%.200q",
				what, len(got), 3*strings.Count(got, "\uFFFD"), config.InlineMaxBytes, got)
		}
	}

	got, _ := s.call(t, "pg_query", args("db", "SELECT 1"))
	check("pg_query", got)
	got, _ = s.call(t, "pg_list_connections", map[string]any{})
	check("pg_list_connections", got)
	_, err := s.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "pgglass://db/public/t"})
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("resources/read = %v, want a JSON-RPC error", err)
	}
	check("resources/read", wire.Message+string(wire.Data))
}
