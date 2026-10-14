package tools_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

// An unknown connection's error, listing every configured name and quoting the
// one given, is cut in the middle to the inline limit.
func TestUnknownConnectionErrorStaysWithinTheInlineLimit(t *testing.T) {
	conns := map[string]config.Connection{}
	for i := range 400 {
		conns[fmt.Sprintf("replica-%03d-of-the-reporting-warehouse", i)] = direct(unusedDSN)
	}
	s := connect(t, newConfig(config.RoleCheckWarn, conns))
	const first, last = "replica-000-of-the-reporting-warehouse", "replica-399-of-the-reporting-warehouse"
	listed := `unknown connection "nope" (available: ` + first + ", "
	for _, tc := range []struct {
		tool       string
		args       map[string]any
		start, end string
	}{
		{"pg_query", args("nope", "SELECT 1"), listed, ", " + last + ")"},
		{"pg_query", fanoutArgs([]string{first, "nope"}, "SELECT 1"), listed, ", " + last + ")"},
		{"pg_diff_schema", diffArgs(first, "nope"), listed, ", " + last + ")"},
		{"pg_schema", map[string]any{"connection": strings.Repeat("x", 20000)}, `unknown connection "xxx`, ", " + last + ")"},
	} {
		got := s.fails(t, tc.tool, tc.args)
		if len(got) > config.InlineMaxBytes || !strings.HasPrefix(got, tc.start) || !strings.HasSuffix(got, tc.end) ||
			!strings.Contains(got, " bytes cut]…") {
			t.Errorf("%s: %d bytes:\n%.300s", tc.tool, len(got), got)
		}
	}
}
