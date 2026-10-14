package tools_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

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

// A merged fan-out's footer names every connection a limit cut; with names
// capped at 63 bytes, it and the spill fallback stay within the inline limit.
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
	s := sectionsRoom(t, "tools_fanout_merged_reason", longest, t.TempDir())

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
