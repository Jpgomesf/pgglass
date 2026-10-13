package tools

import (
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// linesOutput gets lines its caller found too large to return inline. Cut
// to 200 characters each, lines of 300 that total just over 10 KB fit, and
// the spill store returned them so, without a file: their ends were lost.
// Now they always go to a spill file, whole.
func TestLinesOutputSpillsLinesWhole(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}}
	var lines []string
	for i := 0; len(strings.Join(lines, "\n")) <= config.InlineMaxBytes; i++ {
		lines = append(lines, fmt.Sprintf("line %02d: %s.", i, strings.Repeat("x", 300)))
	}

	got, err := h.linesOutput("pg_explain", "db", "line", lines, "", config.InlineMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(": %d rows × 1 column, truncated=false.\n", len(lines))
	if !strings.HasPrefix(got, "Result too large to return inline (") || !strings.Contains(got, want) || len(got) > config.InlineMaxBytes {
		t.Fatalf("%d lines totalling %d bytes: got %d bytes:\n%s", len(lines), len(strings.Join(lines, "\n")), len(got), got)
	}
	path := regexp.MustCompile(`(?m)^Full result: (.+)$`).FindStringSubmatch(got)
	if path == nil {
		t.Fatalf("no spill file named:\n%s", got)
	}
	data, err := os.ReadFile(path[1])
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(lines)+1 || records[0][0] != "line" {
		t.Fatalf("%d CSV records, want a header and %d lines", len(records), len(lines))
	}
	for i, line := range lines {
		if records[i+1][0] != line {
			t.Errorf("record %d = %q, want %q", i+1, records[i+1][0], line)
		}
	}
}
