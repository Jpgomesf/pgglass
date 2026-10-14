package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/spill"
)

func TestTableURIEscapesEachNameAndParsesBack(t *testing.T) {
	for _, tc := range []struct {
		ref  tableURI
		want string
	}{
		{tableURI{"prod-app", "app", "orders"}, "pgglass://prod-app/app/orders"},
		{tableURI{"my db", "Odd Schema", "a/b c?#%"}, "pgglass://my%20db/Odd%20Schema/a%2Fb%20c%3F%23%25"},
		{tableURI{"db", "public", "café"}, "pgglass://db/public/caf%C3%A9"},
		{tableURI{"db.v2", "s_1", "x~y+z"}, "pgglass://db.v2/s_1/x~y%2Bz"},
		{tableURI{"a:b@c", "s;t,u", "v=w&x"}, "pgglass://a%3Ab%40c/s%3Bt%2Cu/v%3Dw%26x"},
		{tableURI{"db", "app", "line\nbreak"}, "pgglass://db/app/line%0Abreak"},
	} {
		got := tc.ref.String()
		if got != tc.want {
			t.Errorf("%+v = %s, want %s", tc.ref, got, tc.want)
		}
		if back, ok := parseTableURI(got); !ok || back != tc.ref {
			t.Errorf("parseTableURI(%s) = %+v, %t; want %+v", got, back, ok, tc.ref)
		}
	}
}

func TestTableURIKeepsOnlyUnreservedBytes(t *testing.T) {
	for c := range 256 {
		s := string([]byte{byte(c)})
		want := fmt.Sprintf("%%%02X", c)
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
			want = s
		}
		if got := strings.TrimPrefix(tableURI{"c", "s", s}.String(), "pgglass://c/s/"); got != want {
			t.Errorf("byte %#x = %q, want %q", c, got, want)
		}
	}
}

func TestParseTableURIRefusesWhatNamesNoTable(t *testing.T) {
	long := strings.Repeat("t", 64)
	for _, uri := range []string{
		"",
		"pgglass://",
		"pgglass://db/app",
		"pgglass://db/app/orders/columns",
		"postgres://db/app/orders",
		"PGGLASS://db/app/orders",
		"pgglass:db/app/orders",
		"pgglass://db//orders",
		"pgglass://db/app/",
		"pgglass://db/app/%zz",
		"pgglass://db/app/%4",
		"pgglass://db/%00/orders",
		"pgglass://db/app/a%00b",
		"pgglass://db/app/%FF",
		"pgglass://db/app/" + long,
		"pgglass://db/" + long + "/orders",
	} {
		if ref, ok := parseTableURI(uri); ok {
			t.Errorf("parseTableURI(%q) = %+v, want it refused", uri, ref)
		}
	}
	name := strings.Repeat("é", 31) + "x"
	if ref, ok := parseTableURI(tableURI{"db", name, name}.String()); !ok || ref.schema != name || ref.table != name {
		t.Errorf("names of 63 bytes = %+v, %t; want them parsed", ref, ok)
	}
}

func TestResourceErrorsFollowTheMCPShapeWithinTheInlineLimit(t *testing.T) {
	h := &Handlers{spill: &spill.Store{InlineMaxBytes: config.InlineMaxBytes}}
	const uri = "pgglass://db/app/orders"
	notice := "role check: role ro: 1 finding(s)\n  warning createdb: can create databases\n\n"
	for _, err := range []error{errNoResource, &dbError{fmt.Errorf("relation %q.%q %w", "app", "orders", introspect.ErrNotFound)}} {
		var wire *jsonrpc.Error
		if !errors.As(h.resourceError(uri, err, &call{notice: notice}), &wire) || wire.Code != jsonrpc.CodeInvalidParams ||
			wire.Message != notice+"Resource not found" || string(wire.Data) != `{"uri":"`+uri+`"}` {
			t.Errorf("resourceError(%v) = %+v, want resource not found after the notice", err, wire)
		}
	}

	var wire *jsonrpc.Error
	if !errors.As(h.resourceError(uri, quotingError(20000), &call{notice: notice}), &wire) || wire.Code != jsonrpc.CodeInternalError ||
		len(wire.Message) > config.InlineMaxBytes || !strings.HasPrefix(wire.Message, notice+`ERROR: invalid input syntax for type integer: "yyy`) ||
		!strings.Contains(wire.Message, " bytes cut]…") || !strings.HasSuffix(wire.Message, "\nHINT: Cast it to text first.") {
		t.Errorf("resourceError of a long error = %+v", wire)
	}
}

func TestURIDataIsTheURIAsJSONWithinItsLimit(t *testing.T) {
	for _, uri := range []string{
		"pgglass://db/app/orders",
		"pgglass://a<b>&\"c\\/app/orders",
		strings.Repeat("\x01", 5000),
		strings.Repeat("é", 3000),
		strings.Repeat("<", 4000),
		strings.Repeat("u", 20000),
	} {
		for _, limit := range []int{config.InlineMaxBytes / 2, config.InlineMaxBytes} {
			data := uriData(uri, limit)
			var got struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal(data, &got); err != nil || len(data) > limit {
				t.Errorf("uriData of a %d-byte URI within %d = %d bytes, %v", len(uri), limit, len(data), err)
				continue
			}
			if whole, _ := json.Marshal(map[string]string{"uri": uri}); len(whole) <= limit && got.URI != uri {
				t.Errorf("uriData of a %d-byte URI within %d cut a URI that fits: %.80q", len(uri), limit, got.URI)
			}
			if !strings.HasPrefix(got.URI, uri[:4]) || !strings.HasSuffix(got.URI, uri[len(uri)-4:]) {
				t.Errorf("uriData of a %d-byte URI within %d = %.80q, want its start and end", len(uri), limit, got.URI)
			}
		}
	}
}
