package render_test

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func str(v string) *string { return &v }

const goldenMaxCell = 20

func fixture() *result.Result {
	return &result.Result{
		Columns: []result.Column{
			{Name: "id", TypeOID: pgtype.Int4OID, TypeName: "int4"},
			{Name: "note", TypeOID: pgtype.TextOID, TypeName: "text"},
			{Name: "note", TypeOID: pgtype.TextOID, TypeName: "text"},
			{Name: "amount", TypeOID: pgtype.NumericOID, TypeName: "numeric"},
			{Name: "ok", TypeOID: pgtype.BoolOID, TypeName: "bool"},
			{Name: "doc", TypeOID: pgtype.JSONBOID, TypeName: "jsonb"},
		},
		Rows: [][]*string{
			{str("1"), nil, str(""), str("12.50"), str("t"), str(`{"a": [1, 2]}`)},
			{str("2"), str("NULL"), str(`\N`), str("NaN"), str("f"), nil},
			{str("3"), str("tab\there"), str("two\nlines\r\n"), str("-0.001"), nil, str(`"x<y & z"`)},
			{str("4"), str("pipe|here"), str(`say "hi", ok`), str("-Infinity"), str("t"), str("null")},
			{str("5"), str(`back\slash`), str("café ☕ 日本語"), str("123456789012345678901234567890"), str("f"), str("[true, null]")},
			{str("6"), str("a cell wider than twenty chars"), str("日本語のテキストは二十文字を超えると切られます"), nil, str("t"), str(`{"k": "v"}`)},
		},
	}
}

func TestGolden(t *testing.T) {
	for f, file := range map[render.Format]string{
		render.TSV:      "fixture.tsv",
		render.CSV:      "fixture.csv",
		render.Markdown: "fixture.md",
		render.JSON:     "fixture.json",
	} {
		t.Run(string(f), func(t *testing.T) {
			got, err := render.Render(fixture(), f, goldenMaxCell)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", file)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("Render(%s) differs from %s (rerun with -update to accept):\ngot:\n%s\nwant:\n%s", f, path, got, want)
			}
		})
	}
}

func renderOne(t *testing.T, f render.Format, col result.Column, v *string) string {
	t.Helper()
	out, err := render.Render(&result.Result{Columns: []result.Column{col}, Rows: [][]*string{{v}}}, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryFormatKeepsNullEmptyAndTextNullApart(t *testing.T) {
	col := result.Column{Name: "v", TypeOID: pgtype.TextOID, TypeName: "text"}
	for _, f := range []render.Format{render.TSV, render.CSV, render.Markdown, render.JSON} {
		null := renderOne(t, f, col, nil)
		empty := renderOne(t, f, col, str(""))
		text := renderOne(t, f, col, str("NULL"))
		if null == empty || null == text || empty == text {
			t.Errorf("%s: NULL %q, empty %q and 'NULL' %q are not all distinct", f, null, empty, text)
		}
	}
}

func TestMaxCellZeroKeepsEveryCharacter(t *testing.T) {
	wide := strings.Repeat("é", 5000)
	col := result.Column{Name: "v", TypeOID: pgtype.TextOID, TypeName: "text"}
	for _, f := range []render.Format{render.TSV, render.CSV, render.Markdown, render.JSON} {
		if out := renderOne(t, f, col, str(wide)); !strings.Contains(out, wide) || strings.Contains(out, "…") {
			t.Errorf("%s: maxCell 0 changed a wide cell", f)
		}
	}
}

func TestMaxCellCutsLongCellsByCharacter(t *testing.T) {
	res := &result.Result{
		Columns: []result.Column{{Name: "v", TypeOID: pgtype.TextOID, TypeName: "text"}},
		Rows:    [][]*string{{str("añb")}, {str("añbc")}, {str("añbcdé")}},
	}
	got, err := render.Render(res, render.TSV, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := "v\nañb\nañbc\nañbc…[+2 chars]\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEmptyResults(t *testing.T) {
	noRows := &result.Result{Columns: []result.Column{{Name: "a", TypeName: "int4"}, {Name: "b", TypeName: "text"}}}
	noColumns := &result.Result{Rows: [][]*string{{}, {}}}
	tests := []struct {
		name string
		res  *result.Result
		f    render.Format
		want string
	}{
		{"tsv no rows", noRows, render.TSV, "a\tb\n"},
		{"csv no rows", noRows, render.CSV, "a,b\r\n"},
		{"markdown no rows", noRows, render.Markdown, "| a | b |\n| --- | --- |\n"},
		{"json no rows", noRows, render.JSON, `{"columns":[{"name":"a","type":"int4"},{"name":"b","type":"text"}],"rows":[]}` + "\n"},
		{"tsv no columns", noColumns, render.TSV, "\n\n\n"},
		{"json no columns", noColumns, render.JSON, `{"columns":[],"rows":[[],[]]}` + "\n"},
		{"json nothing", &result.Result{}, render.JSON, `{"columns":[],"rows":[]}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := render.Render(tt.res, tt.f, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	cols := []result.Column{{Name: "a"}, {Name: "b"}}
	tests := []struct {
		name    string
		res     *result.Result
		f       render.Format
		wantErr string
	}{
		{"unknown format", fixture(), render.Format("xml"), `render: unknown format "xml"`},
		{"empty format", fixture(), render.Format(""), `render: unknown format ""`},
		{"nil result", nil, render.TSV, "render: nil result"},
		{"short row", &result.Result{Columns: cols, Rows: [][]*string{{str("1"), str("2")}, {str("1")}}}, render.TSV,
			"render: row 2 has 1 values for 2 columns"},
		{"long row", &result.Result{Columns: cols, Rows: [][]*string{{str("1"), str("2"), str("3")}}}, render.JSON,
			"render: row 1 has 3 values for 2 columns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := render.Render(tt.res, tt.f, 0)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
			if out != "" {
				t.Errorf("output %q alongside an error", out)
			}
		})
	}
}

func fuzzSeeds(f *testing.F) {
	for _, seed := range []string{"", "NULL", `\N`, `\`, "a\tb", "a\nb", "a\r\nb", "\r", `"`, `""`, ",", "|", `\|`, "é☕", "\xff\xfe"} {
		f.Add(seed, "x", false)
		f.Add("x", seed, true)
	}
}

func twoCells(a, b string, aNull bool) *result.Result {
	first := &a
	if aNull {
		first = nil
	}
	return &result.Result{
		Columns: []result.Column{{Name: a, TypeOID: pgtype.TextOID}, {Name: b, TypeOID: pgtype.TextOID}},
		Rows:    [][]*string{{first, &b}},
	}
}

func FuzzTSVRoundTrip(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, a, b string, aNull bool) {
		res := twoCells(a, b, aNull)
		out, err := render.Render(res, render.TSV, 0)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out, "\n")
		if len(lines) != 3 || lines[2] != "" {
			t.Fatalf("want a header line and one row line, got %q", out)
		}
		header, row := strings.Split(lines[0], "\t"), strings.Split(lines[1], "\t")
		if len(header) != 2 || len(row) != 2 {
			t.Fatalf("want two fields per line, got %q", out)
		}
		fields := []string{header[0], header[1], row[0], row[1]}
		for i, want := range []*string{&a, &b, res.Rows[0][0], &b} {
			if got := unescapeTSV(fields[i]); !sameValue(got, want) {
				t.Errorf("field %d %q reads back as %s, want %s", i, fields[i], show(got), show(want))
			}
		}
	})
}

func FuzzCSVRoundTrip(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, a, b string, aNull bool) {
		out, err := render.Render(twoCells(a, b, aNull), render.CSV, 0)
		if err != nil {
			t.Fatal(err)
		}
		records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
		if err != nil {
			t.Fatalf("encoding/csv cannot read %q: %v", out, err)
		}
		first := a
		if aNull {
			first = ""
		}
		lf := strings.NewReplacer("\r\n", "\n").Replace
		want := [][]string{{lf(a), lf(b)}, {lf(first), lf(b)}}
		if len(records) != 2 || !slices.Equal(records[0], want[0]) || !slices.Equal(records[1], want[1]) {
			t.Errorf("%q reads back as %q, want %q", out, records, want)
		}
	})
}

func FuzzMarkdownKeepsTableShape(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, a, b string, aNull bool) {
		out, err := render.Render(twoCells(a, b, aNull), render.Markdown, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "\r") {
			t.Fatalf("raw carriage return in %q", out)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("want header, rule and one row, got %q", out)
		}
		for _, line := range lines {
			if n := unescapedPipes(line); n != 3 {
				t.Errorf("line %q has %d cell borders, want 3", line, n)
			}
		}
	})
}

func FuzzJSONStaysValid(f *testing.F) {
	oids := []uint32{pgtype.TextOID, pgtype.Int4OID, pgtype.Int8OID, pgtype.Float8OID, pgtype.NumericOID,
		pgtype.BoolOID, pgtype.JSONOID, pgtype.JSONBOID}
	for _, seed := range []string{"", "1", "-0", "1e+100", "NaN", "Infinity", "01", "1.", "t", "f", "true",
		`{"a": 1}`, `{"a": `, "null", "\xff", "<&>", "\u2028"} {
		for i := range oids {
			f.Add(i, seed, 0)
			f.Add(i, seed, 2)
		}
	}
	f.Fuzz(func(t *testing.T, oid int, v string, maxCell int) {
		if oid < 0 || oid >= len(oids) || maxCell < 0 || maxCell > 1000 {
			t.Skip()
		}
		res := &result.Result{
			Columns: []result.Column{{Name: v, TypeOID: oids[oid]}},
			Rows:    [][]*string{{&v}, {nil}},
		}
		out, err := render.Render(res, render.JSON, maxCell)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid([]byte(out)) {
			t.Errorf("invalid JSON for %q: %s", v, out)
		}
	})
}

func unescapeTSV(field string) *string {
	if field == `\N` {
		return nil
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] != '\\' || i+1 == len(field) {
			b.WriteByte(field[i])
			continue
		}
		i++
		switch field[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(field[i])
		}
	}
	s := b.String()
	return &s
}

func unescapedPipes(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n
}

func sameValue(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func show(v *string) string {
	if v == nil {
		return "NULL"
	}
	return `"` + *v + `"`
}
