package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_schema cuts each comment to 200 characters, so listings of long comments
// stay within max_bytes instead of failing.
func TestSchemaOfLongCommentsIsCutNotRefused(t *testing.T) {
	t.Parallel()
	const name = "tools_schema_long_comments"
	testdb.CreateDatabase(t, name, `CREATE SCHEMA s;
		DO $$ BEGIN FOR i IN 1..500 LOOP
			EXECUTE format('CREATE TABLE s.t%s (id int)', lpad(i::text, 3, '0'));
			EXECUTE format('COMMENT ON TABLE s.t%s IS %L', lpad(i::text, 3, '0'), repeat('c', 2200));
		END LOOP; END $$;
		CREATE SCHEMA w;
		DO $$ BEGIN
			EXECUTE 'CREATE TABLE w.wide (' || (SELECT string_agg(format('c%s text', lpad(i::text, 3, '0')), ', ') FROM generate_series(1, 300) i) || ')';
			FOR i IN 1..300 LOOP
				EXECUTE format('COMMENT ON COLUMN w.wide.c%s IS %L', lpad(i::text, 3, '0'), repeat('m', 3600));
			END LOOP;
		END $$;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"db": direct(testdb.DSN(t, testdb.Superuser, name)),
	}))

	for _, tc := range []struct {
		args          map[string]any
		comment, note string
	}{
		{map[string]any{"connection": "db", "schema": "s"}, " -- " + strings.Repeat("c", 200) + "...[+2000 chars]\n",
			" of 500 lines shown (output limit 10000 bytes): use pg_search to find names"},
		{map[string]any{"connection": "db", "schema": "w", "table": "wide"}, " -- " + strings.Repeat("m", 200) + "...[+3400 chars]\n",
			" of 301 lines shown (output limit 10000 bytes): use pg_search to find names"},
	} {
		got, isErr := s.call(t, "pg_schema", tc.args)
		if isErr || len(got) > config.InlineMaxBytes || !strings.HasSuffix(got, tc.note) || !strings.Contains(got, tc.comment) {
			t.Errorf("pg_schema %v: error %v, %d bytes, ends:\n%s", tc.args, isErr, len(got), got[max(0, len(got)-400):])
		}
	}
}
