// Package render turns a result.Result into text for tool output and spill
// files.
//
// Every format keeps SQL NULL apart from the empty string:
//
//   - tsv follows the COPY text format: tab, newline, carriage return and
//     backslash are written \t, \n, \r and \\, and NULL is \N.
//   - csv follows RFC 4180, with CRLF line breaks. As in COPY ... CSV, NULL is
//     an unquoted empty field and the empty string is a quoted one ("").
//   - markdown is a GitHub table. Backslash, pipe, newline and carriage return
//     are written \\, \|, \n and \r. NULL is NULL, and a value that is the
//     text NULL is written \NULL so the two stay apart.
//   - json is one line: {"columns":[{"name":..,"type":..}],"rows":[[..]]}.
//     Columns form an array, so duplicate names survive. Integer, float and
//     numeric values are numbers (NaN and Infinity stay strings), booleans
//     are true and false, json and jsonb values are embedded as JSON, NULL is
//     null, and everything else, including any value cut by maxCell, is a
//     string.
package render

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// Format is an output format: one of config.Formats, or CSV for spill files.
type Format string

// Formats.
const (
	TSV      Format = config.FormatTSV
	JSON     Format = config.FormatJSON
	Markdown Format = config.FormatMarkdown
	CSV      Format = "csv"
)

// Render renders the header and rows of res in format f, and nothing else:
// no row counts or truncation notes. NULL renders distinctly from the empty
// string. maxCell > 0 truncates longer cell values to maxCell characters
// followed by "…[+N chars]", N being the number of characters dropped; 0
// disables cell truncation. Every rendering ends with a line break. An
// unknown format is an error, and so is a row whose length differs from the
// number of columns.
func Render(res *result.Result, f Format, maxCell int) (string, error) {
	if err := checkShape(res); err != nil {
		return "", err
	}
	switch f {
	case TSV:
		return tsvFormat.render(res, maxCell), nil
	case CSV:
		return csvFormat.render(res, maxCell), nil
	case Markdown:
		return markdownFormat.render(res, maxCell), nil
	case JSON:
		return renderJSON(res, maxCell)
	default:
		return "", fmt.Errorf("render: unknown format %q", f)
	}
}

// checkShape rejects a nil result and rows that do not match the columns.
func checkShape(res *result.Result) error {
	if res == nil {
		return errors.New("render: nil result")
	}
	for i, row := range res.Rows {
		if len(row) != len(res.Columns) {
			return fmt.Errorf("render: row %d has %d values for %d columns", i+1, len(row), len(res.Columns))
		}
	}
	return nil
}

// lineFormat is a format with a header line followed by one line per row.
type lineFormat struct {
	open, sep, end string               // written before, between and after the cells of a line
	rule           string               // when set, a line of rule cells follows the header
	name           func(string) string  // encodes a column name
	value          func(*string) string // encodes a cell value; nil is NULL
}

var (
	tsvEscaper      = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)
	markdownEscaper = strings.NewReplacer(`\`, `\\`, "|", `\|`, "\n", `\n`, "\r", `\r`)

	tsvFormat      = lineFormat{sep: "\t", end: "\n", name: tsvEscaper.Replace, value: tsvValue}
	csvFormat      = lineFormat{sep: ",", end: "\r\n", name: csvQuote, value: csvValue}
	markdownFormat = lineFormat{
		open: "| ", sep: " | ", end: " |\n", rule: "---",
		name: markdownEscaper.Replace, value: markdownValue,
	}
)

func (lf lineFormat) render(res *result.Result, maxCell int) string {
	var b strings.Builder
	lf.line(&b, len(res.Columns), func(i int) string { return lf.name(res.Columns[i].Name) })
	if lf.rule != "" {
		lf.line(&b, len(res.Columns), func(int) string { return lf.rule })
	}
	for _, row := range res.Rows {
		lf.line(&b, len(row), func(i int) string { return lf.value(clip(row[i], maxCell)) })
	}
	return b.String()
}

func (lf lineFormat) line(b *strings.Builder, n int, cell func(int) string) {
	b.WriteString(lf.open)
	for i := range n {
		if i > 0 {
			b.WriteString(lf.sep)
		}
		b.WriteString(cell(i))
	}
	b.WriteString(lf.end)
}

func tsvValue(v *string) string {
	if v == nil {
		return `\N`
	}
	return tsvEscaper.Replace(*v)
}

func csvValue(v *string) string {
	if v == nil {
		return ""
	}
	return csvQuote(*v)
}

// csvQuote quotes s when RFC 4180 requires it, and when s is empty so that it
// reads back as the empty string rather than as NULL.
func csvQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, ",\"\r\n") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func markdownValue(v *string) string {
	switch {
	case v == nil:
		return "NULL"
	case *v == "NULL":
		return `\NULL`
	default:
		return markdownEscaper.Replace(*v)
	}
}

// clip truncates a cell value; NULL stays NULL.
func clip(v *string, limit int) *string {
	if v == nil {
		return nil
	}
	if s, cut := truncate(*v, limit); cut {
		return &s
	}
	return v
}

// truncate cuts s to limit characters followed by "…[+N chars]", N being the
// number of characters dropped, and reports whether it did. limit <= 0
// never cuts.
func truncate(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	total := utf8.RuneCountInString(s)
	if total <= limit {
		return s, false
	}
	end, kept := len(s), 0
	for i := range s {
		if kept == limit {
			end = i
			break
		}
		kept++
	}
	return s[:end] + "…[+" + strconv.Itoa(total-limit) + " chars]", true
}
