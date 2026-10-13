package tools_test

import (
	"os"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_health returned its lines through the spill store's Output, which cuts
// every inline cell to 200 characters, so a finding longer than that lost
// its end with no file to read it from: the unused index below, on a table
// whose schema, name and index name take 63 characters each, came back as
// "... on hhhhh…[+70 chars]", without its table or size, though the whole
// result takes well under InlineMaxBytes.
func TestHealthReturnsLongFindingsWhole(t *testing.T) {
	t.Parallel()
	const name = "tools_health_whole"
	schema, table, index := strings.Repeat("h", 63), strings.Repeat("t", 63), strings.Repeat("i", 63)
	testdb.CreateDatabase(t, name, "CREATE SCHEMA "+schema+";\n"+
		"CREATE TABLE "+schema+"."+table+" (v int);\n"+
		"CREATE INDEX "+index+" ON "+schema+"."+table+" (v);")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	got := s.ok(t, "pg_health", map[string]any{"connection": "db"})
	want := schema + "." + index + " on " + schema + "." + table + ": "
	if len(got) > config.InlineMaxBytes {
		t.Fatalf("pg_health returned %d bytes, limit %d", len(got), config.InlineMaxBytes)
	}
	if strings.Contains(got, want) {
		return // whole, inline
	}
	if !strings.Contains(got, "Full result: ") {
		t.Fatalf("the unused index finding is cut and no spill file holds it whole:\n%s", got)
	}
	data, err := os.ReadFile(spillPath(t, got))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("the spill file does not hold the unused index finding whole:\n%s", data)
	}
}
