package spill

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/result"
)

// A summary says what it does of the file when that fits in its limit, with
// no column when not one fits. Below that, it said it all the same, past its
// limit: a fanned-out section's share can be smaller. Now it only names the
// file and counts the rows, past its limit only when that alone does not fit.
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
	fits := len(header(100)) // at any limit of 3 digits
	if len(header(fits)) != fits {
		t.Fatalf("the header takes %d bytes, not 3 digits", fits)
	}
	pointer := "Full result: " + path + "\n(50 rows; truncated: max_rows=50)"
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{fits, header(fits)}, // and not one column
		{fits - 1, pointer},
		{len(pointer), pointer},
		{10, pointer},
	} {
		if got, err := summary(res, path, 6000, tc.limit); err != nil || got != tc.want {
			t.Errorf("limit %d = %v:\n%s\nwant\n%s", tc.limit, err, got, tc.want)
		}
	}
}
