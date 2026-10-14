package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// Before revision 2026-07-28 the server asks the user during the call, and a
// client that fails to ask ends the call with its own error, which the
// protocol error quotes: however long that error, the protocol error keeps to
// 10 KB, as every error does but those the SDK returns before pgglass sees
// the request.
func TestElicitationFailureStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"prod": confirmed(unusedDSN)})
	s := connectClient(t, cfg, t.TempDir(), "2025-06-18", &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return nil, errors.New("the form could not be shown: " + strings.Repeat("x", 30_000))
		},
	})

	_, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: args("prod", "SELECT 1")})
	wire, ok := errors.AsType[*jsonrpc.Error](err)
	if !ok {
		t.Fatalf("a client that fails to ask: %v, want a protocol error", err)
	}
	if size := len(wire.Message) + len(wire.Data); size > config.InlineMaxBytes ||
		!strings.HasPrefix(wire.Message, `multi-round-trip: fulfilling input request "confirm-`) ||
		!strings.Contains(wire.Message, " bytes cut]…") || !strings.HasSuffix(wire.Message, "xxxx") {
		t.Errorf("a client that fails to ask: a protocol error of %d bytes, limit %d, want it cut in the middle:\n%.200s", size, config.InlineMaxBytes, wire.Message)
	}
}
