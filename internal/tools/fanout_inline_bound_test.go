package tools_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// boundColumn is the name of column c of table w in database i of
// TestFanoutSectionsStayWithinTheInlineLimit: 55 characters, and different in
// each database.
func boundColumn(i, c int) string {
	return fmt.Sprintf("%s_%d_%d", strings.Repeat("c", 50), i, c)
}

// pg_query fanned out to connections whose columns differ gives each its own
// section, and the sections share the inline limit. A section's summary
// listed its first 20 columns however long their names were, so ten sections
// of twenty 55-character names took about 17 kB. Now a summary lists as many
// columns as fit, and the sections fit together; each spill file holds every
// column and row.
func TestFanoutSectionsStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	conns := map[string]config.Connection{}
	var names []string
	for i := range 10 {
		cols, values := make([]string, 20), make([]string, 20)
		for c := range cols {
			cols[c], values[c] = boundColumn(i, c)+" text", "'v' || g"
		}
		db := "tools_fanout_bound_" + strconv.Itoa(i)
		testdb.CreateDatabase(t, db, "CREATE TABLE w ("+strings.Join(cols, ", ")+");\n"+
			"INSERT INTO w SELECT "+strings.Join(values, ", ")+" FROM generate_series(1, 50) AS g;")
		name := "db" + strconv.Itoa(i)
		conns[name] = direct(testdb.DSN(t, testdb.Superuser, db))
		names = append(names, name)
	}
	s := connect(t, newConfig(config.RoleCheckOff, conns))

	text := s.ok(t, "pg_query", fanoutArgs(names, "SELECT * FROM w"))
	if len(text) > config.InlineMaxBytes || !strings.HasPrefix(text, "Columns differ across connections, so each has its own result.\n\n") {
		t.Fatalf("fan-out to 10 connections returned %d bytes, limit %d:\n%s", len(text), config.InlineMaxBytes, text)
	}
	files := regexp.MustCompile(`(?m)^Full result: (.+)$`).FindAllStringSubmatch(text, -1)
	if len(files) != 10 {
		t.Fatalf("%d spill files named, want one per connection:\n%s", len(files), text)
	}
	for i, file := range files {
		if !strings.Contains(text, "\n\n"+names[i]+":\nResult too large to return inline (") {
			t.Errorf("no summary under %s:\n%s", names[i], text)
		}
		records := readSpill(t, file[1], s.spillDir)
		if len(records) != 51 || len(records[0]) != 20 || records[0][19] != boundColumn(i, 19) || records[50][19] != "v50" {
			t.Errorf("%s: spill file holds %d records of %d fields, want a header and 50 rows of 20", names[i], len(records), len(records[0]))
		}
	}
}
