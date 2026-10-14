package tools_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_search reads its matches as one catalog value, which max_bytes bounds,
// and a function matches with its identity arguments: 200 functions of 100
// arguments with 63-byte names take that value past max_bytes, and matches
// over 10 KB must go to a spill file rather than fail the call.
func TestSearchSpillsMatchesWhoseSignaturesExceedMaxBytes(t *testing.T) {
	t.Parallel()
	const db = "tools_search_signatures_max_bytes"
	testdb.CreateDatabase(t, db, `DO $$ BEGIN FOR i IN 1..210 LOOP
		EXECUTE format('CREATE SCHEMA s%s', i);
		EXECUTE format('CREATE FUNCTION s%s.lookup(%s) RETURNS int LANGUAGE sql AS ''SELECT 1''', i,
			(SELECT string_agg(format('%I text', rpad('argument_' || g, 63, 'x')), ', ') FROM generate_series(1, 100) g));
	END LOOP; END $$;`)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, db))}))

	got, isErr := s.call(t, "pg_search", map[string]any{"connection": "db", "pattern": "lookup", "limit": 200})
	if isErr || !strings.HasPrefix(got, "Result too large to return inline") || len(got) > config.InlineMaxBytes {
		t.Errorf("pg_search of 200 functions of 100 long arguments: isError %t, %d bytes:\n%.300s", isErr, len(got), got)
	}
}
