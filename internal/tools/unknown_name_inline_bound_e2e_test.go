package tools_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// A tools/call or prompts/get that names no tool or prompt fails within the
// inline limit, however long the name its error echoes and however much
// quoting it takes. A name that fits is echoed whole.
func TestUnknownNamesStayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(unusedDSN)}))
	for _, name := range []string{"pg_nope", strings.Repeat("x", 20000), strings.Repeat("<\x01\"", 4000)} {
		_, toolErr := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name})
		_, promptErr := s.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: name})
		for kind, err := range map[string]error{"tool": toolErr, "prompt": promptErr} {
			var wire *jsonrpc.Error
			if !errors.As(err, &wire) {
				t.Errorf("%s of an unknown %d-byte name: %v, want a JSON-RPC error", kind, len(name), err)
				continue
			}
			whole := fmt.Sprintf("unknown %s %q", kind, name)
			cut := len(whole) > config.InlineMaxBytes
			if size := len(wire.Message) + len(wire.Data); wire.Code != jsonrpc.CodeInvalidParams || size > config.InlineMaxBytes ||
				!strings.HasPrefix(wire.Message, whole[:20]) || !strings.HasSuffix(wire.Message, whole[len(whole)-20:]) ||
				cut != strings.Contains(wire.Message, " bytes cut]…") || !cut && wire.Message != whole {
				t.Errorf("%s of an unknown %d-byte name: code %d, %d bytes of error, want the %d-byte error within %d, cut in the middle: %t\n%.200q",
					kind, len(name), wire.Code, size, len(whole), config.InlineMaxBytes, cut, wire.Message)
			}
		}
	}
}
