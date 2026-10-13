package tools_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// sectionsRoomFixture lets ro_clean write twelve long-named tables, so its
// role check reports a table_write warning of about 1.2 kB, and gives each
// of the schemas s0 to s9 a relation t: in s0 a view whose cast fails
// quoting a 20,000-character value, in s1 to s9 a table whose one column is
// named after its schema, holding 100 rows of 100 characters.
func sectionsRoomFixture() string {
	var b strings.Builder
	b.WriteString(writableTables() + "\n")
	b.WriteString("CREATE SCHEMA s0;\nCREATE VIEW s0.t AS SELECT (repeat('y', 20000) || current_schema())::int AS v;\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&b, "CREATE SCHEMA s%d;\nCREATE TABLE s%d.t AS SELECT repeat('x', 100) AS c%d FROM generate_series(1, 100);\n", i, i, i)
	}
	for i := range 10 {
		fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA s%d TO ro_clean;\nGRANT SELECT ON s%d.t TO ro_clean;\n", i, i)
	}
	return b.String()
}

// sectionsRoom creates database db from sectionsRoomFixture and connects to
// it under role_check warn, as ro_clean, through a connection named names[i]
// whose default_schema is s<i>.
func sectionsRoom(t *testing.T, db string, names []string) *session {
	t.Helper()
	testdb.CreateDatabase(t, db, sectionsRoomFixture())
	conns := map[string]config.Connection{}
	for i, name := range names {
		conns[name] = config.Connection{Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, db), DefaultSchema: fmt.Sprintf("s%d", i)}
	}
	return connect(t, newConfig(config.RoleCheckWarn, conns))
}

// checkSpilledRows checks that the spill files at paths hold every row of
// the tables of s1 to s9, by the name of their column.
func checkSpilledRows(t *testing.T, s *session, paths []string) {
	t.Helper()
	rows := map[string]int{}
	for _, path := range paths {
		records := readSpill(t, path, s.spillDir)
		for _, r := range records[1:] {
			if r[len(r)-1] == strings.Repeat("x", 100) {
				rows[records[0][len(r)-1]]++
			}
		}
	}
	for i := 1; i < 10; i++ {
		if col := fmt.Sprintf("c%d", i); rows[col] != 100 {
			t.Errorf("spill files hold %d rows of %s, want 100", rows[col], col)
		}
	}
}

// A fanned-out pg_query whose columns differ gives each connection that
// succeeded a section of its own, and the sections share what the role
// check's notices and the failed connections' lines leave of the inline
// limit. A section that spilled said what it does of its file in about 430
// bytes however small its share, so under role_check warn ten connections
// whose role has a table_write finding (the notices take their half), one
// of them failing with an error that quotes a long value (its line takes
// half of the rest) and nine sections that spill (shares of about 256 bytes)
// returned 11,557 bytes. Now such a section only names its file and counts
// its rows, and the result names every file.
func TestFanoutSectionsAfterNoticesAndFailuresStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("warehouse-%d", i))
	}
	s := sectionsRoom(t, "tools_fanout_sections_room", names)

	got := s.ok(t, "pg_query", fanoutArgs(names, "SELECT * FROM t"))
	if len(got) > config.InlineMaxBytes {
		t.Fatalf("fan-out returned %d bytes, limit %d:\n%s", len(got), config.InlineMaxBytes, got)
	}
	if !strings.Contains(got, "\nwarehouse-0: ERROR: invalid input syntax for type integer: \"yyy") {
		t.Errorf("no line for the error of warehouse-0:\n%s", got)
	}
	// Every row of warehouse-1 to warehouse-9 is in a spill file the result names.
	checkSpilledRows(t, s, regexp.MustCompile(regexp.QuoteMeta(s.spillDir)+`/spill-[^\s]+\.csv`).FindAllString(got, -1))
}

// Under connection names of 200 characters, even sections that only name
// their files take more than what the notices and the failure line leave,
// about 3.5 kB of 2.5. The sections then go to one more file, a line each,
// whose summary the result returns in their place: every row is in a spill
// file that file names.
func TestFanoutSectionsUnderLongNamesGoToOneFile(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("%s-%d", strings.Repeat("warehouse", 22), i))
	}
	s := sectionsRoom(t, "tools_fanout_sections_long_names", names)

	got := s.ok(t, "pg_query", fanoutArgs(names, "SELECT * FROM t"))
	if len(got) > config.InlineMaxBytes {
		t.Fatalf("fan-out returned %d bytes, limit %d:\n%s", len(got), config.InlineMaxBytes, got)
	}
	if !strings.Contains(got, "\n"+names[0]+": ERROR: invalid input syntax for type integer: \"yyy") {
		t.Errorf("no line for the error of %s:\n%s", names[0], got)
	}
	lines := readSpill(t, spillPath(t, got), s.spillDir)
	if len(lines) < 2 || lines[1][0] != "Columns differ across connections, so each has its own result." {
		t.Fatalf("the file the result names does not hold the sections:\n%q", lines)
	}
	var files []string
	for _, line := range lines[1:] {
		if file, ok := strings.CutPrefix(line[0], "Full result: "); ok {
			files = append(files, file)
		}
	}
	checkSpilledRows(t, s, files)
}
