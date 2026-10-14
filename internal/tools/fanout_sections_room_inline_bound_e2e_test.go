package tools_test

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

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

func sectionsRoom(t *testing.T, db string, names []string, spillDir string) *session {
	t.Helper()
	testdb.CreateDatabase(t, db, sectionsRoomFixture())
	conns := map[string]config.Connection{}
	for i, name := range names {
		conns[name] = config.Connection{Source: "direct", DSN: testdb.DSN(t, testdb.RoleClean, db), DefaultSchema: fmt.Sprintf("s%d", i)}
	}
	return connectSpillingTo(t, newConfig(config.RoleCheckWarn, conns), spillDir)
}

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

// Fan-out sections share what notices and failure lines leave of the inline
// limit; a spilled section in a small share only names its file and row count.
func TestFanoutSectionsAfterNoticesAndFailuresStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("warehouse-%d", i))
	}
	s := sectionsRoom(t, "tools_fanout_sections_room", names, t.TempDir())

	got := s.ok(t, "pg_query", fanoutArgs(names, "SELECT * FROM t"))
	if len(got) > config.InlineMaxBytes {
		t.Fatalf("fan-out returned %d bytes, limit %d:\n%s", len(got), config.InlineMaxBytes, got)
	}
	if !strings.Contains(got, "\nwarehouse-0: ERROR: invalid input syntax for type integer: \"yyy") {
		t.Errorf("no line for the error of warehouse-0:\n%s", got)
	}
	checkSpilledRows(t, s, regexp.MustCompile(regexp.QuoteMeta(s.spillDir)+`/spill-[^\s]+\.csv`).FindAllString(got, -1))
}

// When even sections that only name their files overflow, under 63-byte names,
// they go to one more file, a line each, whose summary the result returns.
func TestFanoutSectionsUnderLongNamesGoToOneFile(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("%02d-%s", i, strings.Repeat("w", 60)))
	}
	dir := t.TempDir()
	for len(dir) < 600 {
		dir = filepath.Join(dir, strings.Repeat("d", 100))
	}
	s := sectionsRoom(t, "tools_fanout_sections_long_names", names, dir)

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
