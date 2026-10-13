package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/health"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// The report is a result like any other: inline when it fits, else spilled
// to a CSV file and summarized.
func TestHealthResultGoesThroughSpill(t *testing.T) {
	store := &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}
	lines := []health.Line{
		{Section: "connections", Detail: "7 of max_connections 100: idle 6, active 1"},
		{Section: "replication", Detail: "not a replica"},
	}
	got, err := store.OutputWhole("pg_health", "db", healthResult(lines), render.TSV, config.InlineMaxBytes)
	if want := "section\tdetail\nconnections\t7 of max_connections 100: idle 6, active 1\nreplication\tnot a replica\n(2 rows)"; err != nil || got != want {
		t.Errorf("inline = %q, %v\nwant %q", got, err, want)
	}

	var many []health.Line
	for i := range 200 {
		many = append(many, health.Line{Section: "unused_indexes", Detail: fmt.Sprintf("app.index_%03d on app.events: 16 kB", i)})
	}
	got, err = store.OutputWhole("pg_health", "db", healthResult(many), render.TSV, config.InlineMaxBytes)
	if err != nil || !strings.HasPrefix(got, "Result too large to return inline (") ||
		!strings.Contains(got, "200 rows × 2 columns, truncated=false.\nFull result: ") ||
		!strings.Contains(got, "Columns:\ncolumn\ttype\nsection\ttext\ndetail\ttext\n") {
		t.Errorf("spilled = %v\n%s", err, got)
	}
}
