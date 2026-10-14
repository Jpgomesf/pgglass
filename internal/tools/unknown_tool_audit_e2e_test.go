package tools_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// A tools/call that names a tool the server does not have, such as one an
// agent guessed, is a call too: like a resource read the SDK turns away, it
// writes one audit line, with outcome error and the name cut in the middle to
// 63 bytes.
func TestAToolCallNamingNoToolIsAudited(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(unusedDSN)}))
	for _, name := range []string{"pg_tables", strings.Repeat("x", 20_000)} {
		if _, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name}); err == nil {
			t.Fatalf("tools/call of a %d-byte name: no error, want unknown tool", len(name))
		}
	}
	want := []auditLine{
		{Msg: "tool call", Tool: "pg_tables", Outcome: "error"},
		{Msg: "tool call", Tool: strings.Repeat("x", 20) + "…[19960 bytes cut]…" + strings.Repeat("x", 20), Outcome: "error"},
	}
	lines := s.auditLines(t)
	for i := range lines {
		lines[i].DurationMS = nil
	}
	if !slices.Equal(lines, want) {
		t.Errorf("audit lines of tools/call to unknown tools:\n%+v\nwant\n%+v", lines, want)
	}
}
