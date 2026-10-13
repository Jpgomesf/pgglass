package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/spill"
)

// A spilled diff's summary is fitted to the room its first line leaves: in a
// room the whole summary would fill alone, it drops preview rows.
func TestDiffTextFitsTheSummaryUnderItsFirstLine(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir()}}
	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("+ table spilled_table_%03d", i))
	}
	// Rendered with a header and a footer, the lines take more than they do
	// alone, so within that they spill, and the summary has room for all of
	// itself.
	whole, err := h.linesOutput("pg_diff_schema", "prod-staging", "difference", lines, "", len(strings.Join(lines, "\n")))
	if err != nil || !strings.HasPrefix(whole, "Result too large to return inline (") {
		t.Fatalf("linesOutput = %v, want a summary:\n%s", err, whole)
	}

	room := len(whole)
	got, err := h.diffText(diffSchemaArgs{Left: "prod", Right: "staging", Schema: "public"}, 0, lines, room)
	if err != nil {
		t.Fatal(err)
	}
	head := "schema public, prod → staging: 100 differences (- only in prod, + only in staging)\n"
	if len(got) > room || !strings.HasPrefix(got, head+"Result too large to return inline (") {
		t.Errorf("in %d bytes: %d bytes:\n%s", room, len(got), got)
	}
}
