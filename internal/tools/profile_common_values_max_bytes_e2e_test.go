package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// pg_profile reads the statistics of up to 50 columns as one catalog value,
// which max_bytes bounds, and pg_stats keeps common values of up to 1 KB each,
// which JSON writes in up to six bytes a character: the common values of 50
// columns of control characters take that value past max_bytes, and an output
// over 10 KB must go to a spill file rather than fail the call.
func TestProfileSpillsCommonValuesThatExceedMaxBytes(t *testing.T) {
	t.Parallel()
	const db = "tools_profile_common_values_max_bytes"
	cols, values := make([]string, 50), make([]string, 50)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%02d text", i)
		values[i] = "repeat(chr(1), 1000) || (g % 5)"
	}
	testdb.CreateDatabase(t, db, "CREATE TABLE wide ("+strings.Join(cols, ", ")+");\n"+
		"INSERT INTO wide SELECT "+strings.Join(values, ", ")+" FROM generate_series(1, 100) g;\nANALYZE wide;")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, db))}))

	got, isErr := s.call(t, "pg_profile", map[string]any{"connection": "db", "table": "public.wide"})
	if isErr || !strings.Contains(got, "\nResult too large to return inline") || len(got) > config.InlineMaxBytes {
		t.Errorf("pg_profile of 50 columns of 1000 control characters: isError %t, %d bytes:\n%.300q", isErr, len(got), got)
	}
}
