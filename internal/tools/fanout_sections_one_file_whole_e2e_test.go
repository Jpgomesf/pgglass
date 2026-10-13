package tools_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Fanned-out sections that take more than their room go to one more file, a
// line each. A section that fit its share went there as it is returned
// inline, its cells cut to 200 characters, so a longer value was whole in no
// file. A spill directory of 600 bytes makes each spilled section's line
// longer than its share: of ten connections of 63-byte names, two fail,
// seven return 200 rows that spill, and one returns a value of 5,000
// characters, which fit its share cut to 200. Now every section then spills
// to a file of its own, which the file of sections names.
func TestFanoutSectionsInOneFileKeepEveryValueWhole(t *testing.T) {
	t.Parallel()
	const db = "tools_fanout_sections_one_file_whole"
	var fixture strings.Builder
	for i := range 10 {
		fmt.Fprintf(&fixture, "CREATE SCHEMA s%d;\n", i)
		switch {
		case i < 2:
			fmt.Fprintf(&fixture, "CREATE VIEW s%d.t AS SELECT (repeat('y', 20000) || current_schema())::int AS v;\n", i)
		case i == 2:
			fmt.Fprintf(&fixture, "CREATE TABLE s%d.t AS SELECT repeat('k', 5000) AS k;\n", i)
		default:
			fmt.Fprintf(&fixture, "CREATE TABLE s%d.t AS SELECT repeat('x', 150) AS c%d FROM generate_series(1, 200);\n", i, i)
		}
	}
	testdb.CreateDatabase(t, db, fixture.String())
	var names []string
	conns := map[string]config.Connection{}
	for i := range 10 {
		name := fmt.Sprintf("%02d-%s", i, strings.Repeat("w", 60))
		names = append(names, name)
		conns[name] = config.Connection{Source: "direct", DSN: testdb.DSN(t, testdb.Superuser, db), DefaultSchema: fmt.Sprintf("s%d", i)}
	}
	dir := t.TempDir()
	for len(dir) < 600 {
		dir = filepath.Join(dir, strings.Repeat("d", 100))
	}
	s := connectSpillingTo(t, newConfig(config.RoleCheckOff, conns), dir)

	got := s.ok(t, "pg_query", fanoutArgs(names, "SELECT * FROM t"))
	if len(got) > config.InlineMaxBytes {
		t.Errorf("fan-out returned %d bytes, limit %d", len(got), config.InlineMaxBytes)
	}
	if strings.HasPrefix(got, "Columns differ across connections") {
		t.Fatalf("the sections came back inline, where cells are cut by design, not in a file of sections:\n%.300s", got)
	}
	files := []string{spillPath(t, got)}
	whole := false
	for len(files) > 0 {
		records := readSpill(t, files[0], s.spillDir)
		files = files[1:]
		for _, r := range records[1:] {
			if path, ok := strings.CutPrefix(r[0], "Full result: "); ok && records[0][0] == "line" {
				files = append(files, path)
			}
			whole = whole || r[len(r)-1] == strings.Repeat("k", 5000)
		}
	}
	if !whole {
		t.Errorf("no spill file holds the value of 5000 characters of %s whole:\n%s", names[2], got)
	}
}
