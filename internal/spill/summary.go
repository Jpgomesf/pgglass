package spill

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

const (
	previewRows    = 5
	previewMaxCell = 64 // characters kept per cell in a summary
	summaryColumns = 20 // columns a summary lists and previews
)

// summary describes a result written to path instead of being returned:
// inlineSize is what its inline rendering would have taken. It lists the
// first summaryColumns columns with their types, then previews as many of
// the first previewRows rows as fit in limit.
func summary(res *result.Result, path string, inlineSize, limit int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Result too large to return inline (%d bytes, limit %d): %s × %s, truncated=%t",
		inlineSize, limit, count(len(res.Rows), "row"), count(len(res.Columns), "column"), res.Truncated)
	if res.Truncated && res.TruncatedReason != "" {
		fmt.Fprintf(&b, " (%s)", res.TruncatedReason)
	}
	fmt.Fprintf(&b, ".\nFull result: %s\n", path)
	b.WriteString(`It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.` + "\n")
	b.WriteString("Use Read or Grep on the file instead of re-running the query.\n\n")

	shown := min(len(res.Columns), summaryColumns)
	if shown < len(res.Columns) {
		fmt.Fprintf(&b, "Columns (first %d of %d):\n", shown, len(res.Columns))
	} else {
		b.WriteString("Columns:\n")
	}
	types, err := render.Render(columnTypes(res.Columns[:shown]), render.TSV, previewMaxCell)
	if err != nil {
		return "", err
	}
	b.WriteString(types)

	head := b.String()
	for n := min(previewRows, len(res.Rows)); n > 0; n-- {
		rows, err := render.Render(preview(res, n, shown), render.TSV, previewMaxCell)
		if err != nil {
			return "", err
		}
		if out := head + "\nFirst " + count(n, "row") + ":\n" + strings.TrimSuffix(rows, "\n"); len(out) <= limit {
			return out, nil
		}
	}
	return strings.TrimSuffix(head, "\n"), nil
}

// columnTypes returns one row per column of cols: its name and type.
func columnTypes(cols []result.Column) *result.Result {
	out := &result.Result{Columns: []result.Column{{Name: "column"}, {Name: "type"}}}
	for _, c := range cols {
		name, typ := c.Name, c.TypeName
		if typ == "" {
			typ = "oid " + strconv.FormatUint(uint64(c.TypeOID), 10)
		}
		out.Rows = append(out.Rows, []*string{&name, &typ})
	}
	return out
}

// preview returns the first n rows of res with only its first k columns.
func preview(res *result.Result, n, k int) *result.Result {
	out := &result.Result{Columns: res.Columns[:k], Rows: make([][]*string, n)}
	for i, row := range res.Rows[:n] {
		out.Rows[i] = row[:k]
	}
	return out
}
