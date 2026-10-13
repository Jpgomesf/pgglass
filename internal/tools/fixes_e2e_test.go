package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

// pg_schema bounds the catalog queries behind its listings, not only their
// output: a database whose schemas, or a schema whose relations, take more
// than max_bytes of catalog result is still listed, with a note that counts
// them all.
func TestSchemaListingsOfAHugeCatalog(t *testing.T) {
	t.Parallel()
	const name = "tools_huge_catalog"
	testdb.CreateDatabase(t, name, `DO $$ BEGIN
		FOR i IN 1..1000 LOOP
			EXECUTE format('CREATE TABLE t%s (id int)', lpad(i::text, 4, '0'));
		END LOOP;
		FOR i IN 1..2500 LOOP
			EXECUTE format('CREATE SCHEMA s%s', lpad(i::text, 4, '0'));
		END LOOP;
	END $$`)
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))})
	cfg.Defaults.MaxBytes = 100_000 // less than the catalog result of all 1000 relations, or all 2501 schemas
	s := connect(t, cfg)

	for _, tc := range []struct {
		args  map[string]any
		first string
		total int
	}{
		{map[string]any{"connection": "db"}, "public: 1000 relations -- standard public schema", 2501},
		{map[string]any{"connection": "db", "schema": "public"}, "public.t0001: table, 0 bytes", 1000},
	} {
		got := s.ok(t, "pg_schema", tc.args)
		lines := strings.Split(got, "\n")
		note := fmt.Sprintf("… %d of %d lines shown (output limit %d bytes): use pg_search to find names",
			len(lines)-1, tc.total, config.InlineMaxBytes)
		if len(got) > config.InlineMaxBytes || len(lines) < 100 || lines[0] != tc.first || lines[len(lines)-1] != note {
			t.Errorf("pg_schema %v (%d bytes):\n%s", tc.args, len(got), got)
		}
	}
}
