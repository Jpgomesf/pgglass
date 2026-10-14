package spill

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/result"
)

// A summary says all it can of the file within its limit, and below that only
// names the file and counts the rows, past its limit only if that cannot fit.
func TestSummaryNamesTheFileWithinItsLimit(t *testing.T) {
	const path = "/home/me/.cache/pgglass/spill-pg_query-prod-20261012T101530Z-1.csv"
	res := result.Text("note")
	for range 50 {
		res.AddRow(strings.Repeat("x", 100))
	}
	res.Truncated, res.TruncatedReason = true, "max_rows=50"
	header := func(limit int) string {
		return fmt.Sprintf("Result too large to return inline (6000 bytes, limit %d): 50 rows × 1 column, truncated=true (max_rows=50).\n", limit) +
			"Full result: " + path + "\n" +
			`It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.` + "\n" +
			"Use Read or Grep on the file instead of re-running the query."
	}
	fits := len(header(100))
	if len(header(fits)) != fits {
		t.Fatalf("the header takes %d bytes, not 3 digits", fits)
	}
	pointer := "Full result: " + path + "\n(50 rows; truncated: max_rows=50)"
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{fits, header(fits)},
		{fits - 1, pointer},
		{len(pointer), pointer},
		{10, pointer},
	} {
		if got, err := summary(res, path, 6000, tc.limit); err != nil || got != tc.want {
			t.Errorf("limit %d = %v:\n%s\nwant\n%s", tc.limit, err, got, tc.want)
		}
	}
}

// Naming the file and counting the rows keeps to the limit too whenever the
// path fits, cutting the count's note in the middle: under a Linux cache path
// of 2,000 bytes, a fan-out merged from 9 connections cut by max_rows, next to
// a failure line and the role check's findings, has about 2,500 bytes for it.
func TestSummaryFallbackKeepsToTheLimit(t *testing.T) {
	res := &result.Result{Columns: []result.Column{{Name: "connection"}, {Name: "id"}}}
	for range 9000 {
		res.Rows = append(res.Rows, []*string{nil, nil})
	}
	var cut []string
	for range 9 {
		cut = append(cut, strings.Repeat("w", 63)+" max_rows=1000")
	}
	res.Truncated, res.TruncatedReason = true, strings.Join(cut, ", ")
	path := "/" + strings.Repeat("d", 2000) + "/spill-pg_query-wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww-20261014T000000Z-1234567890.csv"
	const limit = 2500

	out, err := summary(res, path, 500_000, limit)
	line := "Full result: " + path + "\n"
	if err != nil || len(out) > limit || !strings.HasPrefix(out, line+"(9000 rows; truncated: www") ||
		!strings.Contains(out, " bytes cut]…") || !strings.HasSuffix(out, "w max_rows=1000)") {
		t.Errorf("summary = %v, %d bytes for a limit of %d:\n%.120s...%s", err, len(out), limit, out, out[max(0, len(out)-200):])
	}
}
