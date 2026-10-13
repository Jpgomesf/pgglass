package tools

import (
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// textColumn is a text column named name.
func textColumn(name string) result.Column {
	return result.Column{Name: name, TypeOID: pgtype.TextOID, TypeName: "text"}
}

// readRecords reads the CSV file at path.
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

// Sections share what the role check's notices and the failure lines leave:
// with the notices at their half of the inline limit, one long failure line
// and nine sections of 10 kB each, every spilled section's summary named its
// file past its share of about 256 bytes, and the text overran its room.
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
	room := config.InlineMaxBytes / 2 // what the notices leave at most
	got, err := h.fanoutText(results, render.TSV, room)
	if err != nil || len(got) > room {
		t.Errorf("fanoutText = %v, %d bytes for a room of %d:\n%s", err, len(got), room, got)
	}
}

// A spilled section names its file even past its share, so under long
// connection names the sections took more than their room: ten names of 300
// characters alone took 3 kB of 2,500 bytes. Now the sections go to one more
// file, a line each, and the result is its summary: it names that file
// alone, which names the file of each connection's result.
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
	// Each section names its connection, then the file that holds its rows.
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
