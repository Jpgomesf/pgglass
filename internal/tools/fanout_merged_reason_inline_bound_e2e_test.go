package tools_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

// parseConnectionNames parses a config.json whose connections have the given
// names, as the server loads it, and returns its error.
func parseConnectionNames(names ...string) error {
	conns := map[string]config.Connection{}
	for _, name := range names {
		conns[name] = direct(unusedDSN)
	}
	doc, err := json.Marshal(config.Config{Connections: conns})
	if err != nil {
		return err
	}
	_, err = config.Parse(doc)
	return err
}

// A fanned-out pg_query whose connections return the same columns merges
// their rows, and its footer names every connection a limit cut. When the
// merged result spills and its summary does not fit, the summary falls back
// to the file and that footer, whatever it takes. Under role_check warn, ten
// connections of 299 characters whose roles have a table_write finding, one
// failing on a long value and nine cut by max_rows, returned about 10.5 kB.
//
// Config now refuses connection names over 63 bytes. Under that cap the
// footer's list of the connections a limit cut takes at most 948 bytes (ten
// names, each with its limit: max_bytes and 19 digits at most), and the
// summary's first part at most 1,271 bytes and its file path. The rows get at
// least 2,499 bytes, what the failures' half and their line break leave of
// the 5,000 the findings leave at least, so that part fits for a path of up
// to 1.2 kB, and the fallback, the path and the footer, fits too.
func TestFanoutMergedTruncationReasonStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	names := func(size int) []string {
		var names []string
		for i := range 10 {
			names = append(names, fmt.Sprintf("%s-%d", strings.Repeat("w", size-2), i))
		}
		return names
	}
	if err := parseConnectionNames(names(299)...); err == nil || !strings.Contains(err.Error(), " is 299 bytes: connection names take at most 63") {
		t.Errorf("config.json with names of 299 bytes: %v", err)
	}
	longest := names(63)
	s := sectionsRoom(t, "tools_fanout_merged_reason", longest)

	// The whole-row reference reads v in s0, whose cast fails, and gives every
	// other connection the same column. max_rows cuts each of them, and their
	// 450 rows spill.
	got := s.ok(t, "pg_query", fanoutArgs(longest, "SELECT t::text IS NOT NULL AS v FROM t", "max_rows", 50))
	if len(got) > config.InlineMaxBytes {
		t.Errorf("fan-out returned %d bytes, limit %d:\n%.200s…%s", len(got), config.InlineMaxBytes, got, got[len(got)-200:])
	}
	if !strings.Contains(got, "\n"+longest[0]+": ERROR: invalid input syntax for type integer: \"yyy") {
		t.Errorf("no line for the error of %s:\n%.300s", longest[0], got)
	}
	var cut []string
	for _, name := range longest[1:] {
		cut = append(cut, name+" max_rows=50")
	}
	if !strings.Contains(got, ": 450 rows × 2 columns, truncated=true ("+strings.Join(cut, ", ")+").\nFull result: ") {
		t.Errorf("the summary does not name every connection the limit cut:\n%s", got)
	}
}
