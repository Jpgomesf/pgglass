package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// A spilled diff's summary is fitted to the room its first line leaves: in a
// room the whole summary would fill alone, it drops preview rows.
func TestDiffTextFitsTheSummaryUnderItsFirstLine(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}}
	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("+ table spilled_table_%03d", i))
	}
	whole, err := h.linesOutput("pg_diff_schema", "prod-staging", "difference", lines, config.InlineMaxBytes)
	if err != nil {
		t.Fatal(err)
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
