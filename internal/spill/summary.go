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
// inlineSize is what its inline rendering would have taken. After what it
// says of the file, it lists the first summaryColumns columns with their
// types, then previews the first previewRows rows, as many of each as fit in
// limit. When what it says of the file does not fit in limit, it only names
// the file, then counts the rows as an inline result does, "Full result:
// <path>" and "(100 rows)", even when that takes more than limit.
func summary(res *result.Result, path string, inlineSize, limit int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Result too large to return inline (%d bytes, limit %d): %s × %s, truncated=%t",
		inlineSize, limit, count(len(res.Rows), "row"), count(len(res.Columns), "column"), res.Truncated)
	if res.Truncated && res.TruncatedReason != "" {
		fmt.Fprintf(&b, " (%s)", res.TruncatedReason)
	}
	fmt.Fprintf(&b, ".\nFull result: %s\n", path)
	b.WriteString(`It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.` + "\n")
	b.WriteString("Use Read or Grep on the file instead of re-running the query.")
	out := b.String()
	if len(out) > limit {
		return "Full result: " + path + "\n" + footer(res), nil
	}

	const gap = "\n\n"
	columns, shown, err := columnList(res.Columns, limit-len(out)-len(gap))
	if err != nil {
		return "", err
	}
	if shown == 0 {
		return out, nil
	}
	out += gap + columns
	rows, err := previewList(res, shown, limit-len(out)-len(gap))
	if err != nil {
		return "", err
	}
	if rows == "" {
		return out, nil
	}
	return out + gap + rows, nil
}

// columnList lists the first summaryColumns of cols with their types, or as
// many as fit in budget bytes, and returns how many it lists: none when not
// one fits.
func columnList(cols []result.Column, budget int) (string, int, error) {
	for k := min(len(cols), summaryColumns); k > 0; k-- {
		heading := "Columns:\n"
		if k < len(cols) {
			heading = fmt.Sprintf("Columns (first %d of %d):\n", k, len(cols))
		}
		types, err := render.Render(columnTypes(cols[:k]), render.TSV, previewMaxCell)
		if err != nil {
			return "", 0, err
		}
		if list := heading + strings.TrimSuffix(types, "\n"); len(list) <= budget {
			return list, k, nil
		}
	}
	return "", 0, nil
}

// previewList previews the first previewRows rows of res with their first k
// columns, as many rows as fit in budget bytes; "" when not one fits.
func previewList(res *result.Result, k, budget int) (string, error) {
	for n := min(previewRows, len(res.Rows)); n > 0; n-- {
		rows, err := render.Render(preview(res, n, k), render.TSV, previewMaxCell)
		if err != nil {
			return "", err
		}
		if list := "First " + count(n, "row") + ":\n" + strings.TrimSuffix(rows, "\n"); len(list) <= budget {
			return list, nil
		}
	}
	return "", nil
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
