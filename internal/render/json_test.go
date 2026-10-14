package render_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

func firstCell(t *testing.T, oid uint32, v *string, maxCell int) string {
	t.Helper()
	res := &result.Result{Columns: []result.Column{{Name: "v", TypeOID: oid}}, Rows: [][]*string{{v}}}
	out, err := render.Render(res, render.JSON, maxCell)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("invalid JSON: %s", out)
	}
	var doc struct {
		Rows [][]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	return string(doc.Rows[0][0])
}

func TestJSONTypesCellsByColumnType(t *testing.T) {
	tests := []struct {
		name    string
		oid     uint32
		v       *string
		maxCell int
		want    string
	}{
		{"int2", pgtype.Int2OID, str("-32768"), 0, `-32768`},
		{"int4", pgtype.Int4OID, str("42"), 0, `42`},
		{"int8 beyond float64 precision", pgtype.Int8OID, str("9223372036854775807"), 0, `9223372036854775807`},
		{"float4 exponent", pgtype.Float4OID, str("1.5e-07"), 0, `1.5e-07`},
		{"float8 exponent", pgtype.Float8OID, str("1e+100"), 0, `1e+100`},
		{"float8 negative zero", pgtype.Float8OID, str("-0"), 0, `-0`},
		{"float8 NaN", pgtype.Float8OID, str("NaN"), 0, `"NaN"`},
		{"float8 Infinity", pgtype.Float8OID, str("Infinity"), 0, `"Infinity"`},
		{"float8 -Infinity", pgtype.Float8OID, str("-Infinity"), 0, `"-Infinity"`},
		{"numeric keeps its scale", pgtype.NumericOID, str("12.50"), 0, `12.50`},
		{"numeric NaN", pgtype.NumericOID, str("NaN"), 0, `"NaN"`},
		{"numeric Infinity", pgtype.NumericOID, str("Infinity"), 0, `"Infinity"`},
		{"numeric NULL", pgtype.NumericOID, nil, 0, `null`},
		{"bool true", pgtype.BoolOID, str("t"), 0, `true`},
		{"bool false", pgtype.BoolOID, str("f"), 0, `false`},
		{"bool NULL", pgtype.BoolOID, nil, 0, `null`},
		{"json is compacted", pgtype.JSONOID, str(`{"b" : 2,  "a" : [1, 2]}`), 0, `{"b":2,"a":[1,2]}`},
		{"jsonb", pgtype.JSONBOID, str(`[1, "two", null, {"x": "<&>"}]`), 0, `[1,"two",null,{"x":"<&>"}]`},
		{"jsonb null is null", pgtype.JSONBOID, str(`null`), 0, `null`},
		{"jsonb string", pgtype.JSONBOID, str(`"text"`), 0, `"text"`},
		{"jsonb NULL", pgtype.JSONBOID, nil, 0, `null`},
		{"text that looks like a number", pgtype.TextOID, str("42"), 0, `"42"`},
		{"text that looks like a bool", pgtype.TextOID, str("true"), 0, `"true"`},
		{"text with markup", pgtype.TextOID, str("<a href='x'>&amp;</a>"), 0, `"<a href='x'>&amp;</a>"`},
		{"text with escapes", pgtype.TextOID, str("tab\there \"q\" back\\slash"), 0, `"tab\there \"q\" back\\slash"`},
		{"text empty", pgtype.TextOID, str(""), 0, `""`},
		{"text NULL", pgtype.TextOID, nil, 0, `null`},
		{"uuid", pgtype.UUIDOID, str("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"), 0, `"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"`},
		{"timestamptz", pgtype.TimestamptzOID, str("2026-10-12 10:15:30+00"), 0, `"2026-10-12 10:15:30+00"`},
		{"text array", pgtype.TextArrayOID, str("{a,NULL}"), 0, `"{a,NULL}"`},
		{"unknown type", 16385, str("pending"), 0, `"pending"`},
		{"malformed int", pgtype.Int4OID, str("0x1F"), 0, `"0x1F"`},
		{"malformed numeric", pgtype.NumericOID, str("1."), 0, `"1."`},
		{"malformed bool", pgtype.BoolOID, str("yes"), 0, `"yes"`},
		{"malformed jsonb", pgtype.JSONBOID, str(`{"a":`), 0, `"{\"a\":"`},
		{"cut number is a string", pgtype.Int8OID, str("9223372036854775807"), 5, `"92233…[+14 chars]"`},
		{"cut jsonb is a string", pgtype.JSONBOID, str(`{"a": 1}`), 3, `"{\"a…[+5 chars]"`},
		{"uncut number at the limit", pgtype.Int4OID, str("12345"), 5, `12345`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstCell(t, tt.oid, tt.v, tt.maxCell); got != tt.want {
				t.Errorf("cell = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestJSONDecodesToTypedValues(t *testing.T) {
	out, err := render.Render(fixture(), render.JSON, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) || strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("want valid JSON on one line, got %q", out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var doc struct {
		Columns []struct{ Name, Type string }
		Rows    [][]any
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	var names, types []string
	for _, c := range doc.Columns {
		names, types = append(names, c.Name), append(types, c.Type)
	}
	if want := []string{"id", "note", "note", "amount", "ok", "doc"}; !slices.Equal(names, want) {
		t.Errorf("column names = %q, want %q (duplicates must survive)", names, want)
	}
	if want := []string{"int4", "text", "text", "numeric", "bool", "jsonb"}; !slices.Equal(types, want) {
		t.Errorf("column types = %q, want %q", types, want)
	}

	wantRows := [][]any{
		{json.Number("1"), nil, "", json.Number("12.50"), true, map[string]any{"a": []any{json.Number("1"), json.Number("2")}}},
		{json.Number("2"), "NULL", `\N`, "NaN", false, nil},
	}
	for i, want := range wantRows {
		if !reflect.DeepEqual(doc.Rows[i], want) {
			t.Errorf("row %d = %#v, want %#v", i, doc.Rows[i], want)
		}
	}
}

func TestJSONKeepsMarkupUnescaped(t *testing.T) {
	out, err := render.Render(fixture(), render.JSON, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"x<y & z"`) || strings.Contains(out, `\`+"u003c") || strings.Contains(out, `\`+"u0026") {
		t.Errorf("want <, > and & written as is, got %s", out)
	}
}
