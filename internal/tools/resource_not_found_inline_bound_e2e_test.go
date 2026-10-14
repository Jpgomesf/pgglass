package tools_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// A resources/read that names no table fails as resource not found within the
// inline limit, however long the URI it echoes in the error's data and however
// much JSON escapes it: whether the table template matches the URI, so that
// the server's handler refuses it, or not, so that the SDK does. A URI that
// fits is echoed whole.
func TestResourceNotFoundStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(unusedDSN)})
	long, escaped := strings.Repeat("u", 20000), strings.Repeat("<\x01\"", 7000)
	for _, tc := range []struct {
		name, uri string
		cut       bool
	}{
		{"unknown connection", "pgglass://nope/app/orders", false},
		{"connection that JSON escapes", "pgglass://a\x01<\"b/app/orders", false},
		{"long connection", "pgglass://" + long + "/app/orders", true},
		{"long table", "pgglass://db/app/" + long, true},
		{"long connection that JSON escapes", "pgglass://" + escaped + "/app/orders", true},
		{"no table", "pgglass://db/app", false},
		{"no table, long schema", "pgglass://db/" + long, true},
		{"another scheme", "file:///" + long, true},
		{"another scheme, JSON escaped", "pgglass://db/" + escaped, true},
		{"another scheme, past the limit once JSON escaped", "file:///" + escaped[:3000], true},
	} {
		_, err := connect(t, cfg).ReadResource(t.Context(), &mcp.ReadResourceParams{URI: tc.uri})
		var wire *jsonrpc.Error
		if !errors.As(err, &wire) {
			t.Errorf("%s: resources/read of a %d-byte URI: %v, want a JSON-RPC error", tc.name, len(tc.uri), err)
			continue
		}
		var data struct {
			URI string `json:"uri"`
		}
		if size := len(wire.Message) + len(wire.Data); wire.Code != jsonrpc.CodeInvalidParams || wire.Message != "Resource not found" ||
			size > config.InlineMaxBytes || json.Unmarshal(wire.Data, &data) != nil {
			t.Errorf("%s: resources/read of a %d-byte URI: code %d, %q and %d bytes of data, want resource not found within %d bytes",
				tc.name, len(tc.uri), wire.Code, wire.Message, len(wire.Data), config.InlineMaxBytes)
			continue
		}
		if tc.cut != strings.Contains(data.URI, " bytes cut]…") || !strings.HasPrefix(data.URI, tc.uri[:12]) ||
			!strings.HasSuffix(data.URI, tc.uri[len(tc.uri)-12:]) || !tc.cut && data.URI != tc.uri {
			t.Errorf("%s: data names %.80q (%d bytes), want the URI, cut in the middle: %t", tc.name, data.URI, len(data.URI), tc.cut)
		}
	}
}
