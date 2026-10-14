package tools

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/spill"
)

func textColumn(name string) result.Column {
	return result.Column{Name: name, TypeOID: pgtype.TextOID, TypeName: "text"}
}

func readRecords(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// Sections share what the role check's notices and the failure lines leave: a
// long failure and nine spilled sections still fit their room.
func TestFanoutSectionsFitWhatNoticesAndFailuresLeave(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}}
	results := []fanoutResult{{conn: "warehouse-0", err: quotingError(20000)}}
	for i := 1; i < 10; i++ {
		values := make([]string, 100)
		for v := range values {
			values[v] = strings.Repeat("x", 100)
		}
		results = append(results, fanoutOK(fmt.Sprintf("warehouse-%d", i), textColumn(fmt.Sprintf("c%d", i)), values...))
	}
	room := config.InlineMaxBytes / 2
	got, err := h.fanoutText(results, render.TSV, room)
	if err != nil || len(got) > room {
		t.Errorf("fanoutText = %v, %d bytes for a room of %d:\n%s", err, len(got), room, got)
	}
}

// Sections past their room go to one more file, a line each, and the result is
// its summary, naming that file alone.
func TestFanoutSectionsPastTheirRoomGoToOneFile(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}}
	var ok []fanoutResult
	for i := range 10 {
		ok = append(ok, fanoutOK(fmt.Sprintf("%s_%d", strings.Repeat("n", 300), i), textColumn(fmt.Sprintf("c%d", i)), fmt.Sprintf("v%d", i)))
	}
	const room = 2500
	got, err := h.fanoutText(ok, render.TSV, room)
	files := regexp.MustCompile(`(?m)^Full result: (.+\.csv)$`).FindAllStringSubmatch(got, -1)
	if err != nil || len(got) > room || len(files) != 1 {
		t.Fatalf("ten sections under long names = %v, %d bytes for a room of %d:\n%s", err, len(got), room, got)
	}
	lines := readRecords(t, files[0][1])
	if len(lines) < 2 || lines[0][0] != "line" || lines[1][0] != "Columns differ across connections, so each has its own result." {
		t.Fatalf("the file holds:\n%q", lines)
	}
	var sections, want []string
	for i, line := range lines {
		if file, isFile := strings.CutPrefix(line[0], "Full result: "); isFile {
			sections = append(sections, lines[i-1][0]+" "+fmt.Sprint(readRecords(t, file)))
		}
	}
	for i, r := range ok {
		want = append(want, fmt.Sprintf("%s: [[c%d] [v%d]]", r.conn, i, i))
	}
	if !slices.Equal(sections, want) {
		t.Errorf("the sections in the file:\n%q\nwant\n%q", sections, want)
	}
}

// Sections that go to one more file each spill to a file of their own first, so
// a value cut inline is whole in some file.
func TestFanoutSectionsPastTheirRoomKeepEveryValueWhole(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 600 {
		dir = filepath.Join(dir, strings.Repeat("d", 100))
	}
	h := &Handlers{spill: &spill.Store{Dir: dir, InlineMaxBytes: config.InlineMaxBytes}}
	long := strings.Repeat("k", 5000)
	rows := slices.Repeat([]string{strings.Repeat("x", 100)}, 20)
	ok := []fanoutResult{fanoutOK("long", textColumn("k"), long), fanoutOK("rows1", textColumn("c1"), rows...), fanoutOK("rows2", textColumn("c2"), rows...)}
	const room = 1000
	got, err := h.fanoutText(ok, render.TSV, room)
	files := regexp.MustCompile(`(?m)^Full result: (.+\.csv)$`).FindAllStringSubmatch(got, -1)
	if err != nil || len(got) > room || len(files) != 1 {
		t.Fatalf("three sections under a long spill path = %v, %d bytes for a room of %d:\n%s", err, len(got), room, got)
	}
	lines := readRecords(t, files[0][1])
	held := map[string][][]string{}
	for i, line := range lines {
		if file, isFile := strings.CutPrefix(line[0], "Full result: "); isFile {
			held[lines[i-1][0]] = readRecords(t, file)
		}
	}
	if records := held["long:"]; len(records) != 2 || records[1][0] != long {
		t.Errorf("no file the sections name holds the 5000 characters of long whole; the file of sections holds:\n%q", lines)
	}
	for _, conn := range []string{"rows1", "rows2"} {
		if records := held[conn+":"]; len(records) != 21 {
			t.Errorf("%s: the file the sections name holds %d records, want a header and 20 rows", conn, len(records))
		}
	}
}
