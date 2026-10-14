package spill

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

const (
	previewRows    = 5
	previewMaxCell = 64
	summaryColumns = 20
)

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

func preview(res *result.Result, n, k int) *result.Result {
	out := &result.Result{Columns: res.Columns[:k], Rows: make([][]*string, n)}
	for i, row := range res.Rows[:n] {
		out.Rows[i] = row[:k]
	}
	return out
}
