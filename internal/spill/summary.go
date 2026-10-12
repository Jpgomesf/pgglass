package spill

import (
	"cmp"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

const (
	previewRows    = 5
	previewMaxCell = 64   // characters kept per cell in a summary
	distinctCap    = 1000 // distinct values counted per column before giving up
)

// summary describes a result written to path instead of being returned:
// inlineSize is what its inline rendering would have taken.
func summary(res *result.Result, path string, inlineSize, limit int) (string, error) {
	stats, err := render.Render(columnStats(res), render.TSV, previewMaxCell)
	if err != nil {
		return "", err
	}
	first := head(res, min(previewRows, len(res.Rows)))
	preview, err := render.Render(first, render.TSV, previewMaxCell)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Result too large to return inline (%d bytes, limit %d): %s × %s, truncated=%t",
		inlineSize, limit, count(len(res.Rows), "row"), count(len(res.Columns), "column"), res.Truncated)
	if res.Truncated && res.TruncatedReason != "" {
		fmt.Fprintf(&b, " (%s)", res.TruncatedReason)
	}
	fmt.Fprintf(&b, ".\nFull result: %s\n", path)
	b.WriteString(`It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.` + "\n")
	b.WriteString("Use Read or Grep on the file instead of re-running the query.\n")
	fmt.Fprintf(&b, "\nColumns:\n%s\nFirst %s:\n%s", stats, count(len(first.Rows), "row"), preview)
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// columnStats returns one row per column of res: its name and type, how many
// values are NULL, how many distinct values the rest hold (">1000" past
// distinctCap), and the least and greatest value for numeric and temporal
// types.
func columnStats(res *result.Result) *result.Result {
	out := &result.Result{Columns: []result.Column{
		{Name: "column"}, {Name: "type"}, {Name: "nulls"}, {Name: "distinct"}, {Name: "min"}, {Name: "max"},
	}}
	for i, c := range res.Columns {
		st := stat{distinct: map[string]struct{}{}, key: keyFor(c.TypeOID)}
		for _, row := range res.Rows {
			st.add(row[i])
		}
		out.Rows = append(out.Rows, st.row(c))
	}
	return out
}

// stat accumulates the statistics of one column.
type stat struct {
	nulls     int
	distinct  map[string]struct{}
	overflow  bool                         // more than distinctCap distinct values
	key       func(string) (sortKey, bool) // orders values; nil when there is no min and max
	seen      bool                         // low and high hold values
	low, high ranked
}

// ranked is a value with its sort key.
type ranked struct {
	key  sortKey
	text string
}

func (st *stat) add(v *string) {
	if v == nil {
		st.nulls++
		return
	}
	st.addDistinct(*v)
	st.addRange(*v)
}

func (st *stat) addDistinct(v string) {
	if st.overflow {
		return
	}
	if _, ok := st.distinct[v]; ok {
		return
	}
	if len(st.distinct) == distinctCap {
		st.overflow = true
		return
	}
	st.distinct[v] = struct{}{}
}

// addRange updates the least and greatest value. A value the key cannot
// parse drops both for the column: no range is better than a wrong one.
func (st *stat) addRange(v string) {
	if st.key == nil {
		return
	}
	k, ok := st.key(v)
	if !ok {
		st.key = nil
		return
	}
	if !st.seen || k.compare(st.low.key) < 0 {
		st.low = ranked{k, v}
	}
	if !st.seen || k.compare(st.high.key) > 0 {
		st.high = ranked{k, v}
	}
	st.seen = true
}

func (st *stat) row(c result.Column) []*string {
	distinct := strconv.Itoa(len(st.distinct))
	if st.overflow {
		distinct = ">" + strconv.Itoa(distinctCap)
	}
	low, high := "", ""
	if st.key != nil && st.seen {
		low, high = st.low.text, st.high.text
	}
	return []*string{ptr(c.Name), ptr(typeLabel(c)), ptr(strconv.Itoa(st.nulls)), ptr(distinct), ptr(low), ptr(high)}
}

func typeLabel(c result.Column) string {
	if c.TypeName != "" {
		return c.TypeName
	}
	return "oid " + strconv.FormatUint(uint64(c.TypeOID), 10)
}

func ptr(s string) *string { return &s }

// sortKey orders numbers and times as Postgres does: -Infinity, then finite
// values, then Infinity, then NaN.
type sortKey struct {
	rank int       // rankNegInf, rankFinite, rankPosInf or rankNaN
	num  *big.Rat  // a finite number
	at   time.Time // a finite time
}

const (
	rankNegInf = iota
	rankFinite
	rankPosInf
	rankNaN
)

func (a sortKey) compare(b sortKey) int {
	switch {
	case a.rank != b.rank || a.rank != rankFinite:
		return cmp.Compare(a.rank, b.rank)
	case a.num != nil:
		return a.num.Cmp(b.num)
	default:
		return a.at.Compare(b.at)
	}
}

// keyFor returns how to order values of type oid, or nil when the column gets
// no min and max: only numeric and temporal types do. Intervals are left out,
// as their text form does not parse simply.
func keyFor(oid uint32) func(string) (sortKey, bool) {
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.Float4OID, pgtype.Float8OID, pgtype.NumericOID:
		return numberKey
	case pgtype.DateOID:
		return timeKey("2006-01-02")
	case pgtype.TimeOID:
		return timeKey("15:04:05")
	case pgtype.TimetzOID:
		return timeKey("15:04:05-07", "15:04:05-07:00", "15:04:05-07:00:00")
	case pgtype.TimestampOID:
		return timeKey("2006-01-02 15:04:05")
	case pgtype.TimestamptzOID:
		return timeKey("2006-01-02 15:04:05-07", "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05-07:00:00")
	default:
		return nil
	}
}

// numberKey parses the text form of an integer, float or numeric value
// exactly, so int8 and numeric values beyond float64 precision still order.
func numberKey(s string) (sortKey, bool) {
	switch s {
	case "-Infinity":
		return sortKey{rank: rankNegInf}, true
	case "Infinity":
		return sortKey{rank: rankPosInf}, true
	case "NaN":
		return sortKey{rank: rankNaN}, true
	}
	n, ok := new(big.Rat).SetString(s)
	return sortKey{rank: rankFinite, num: n}, ok
}

// timeKey parses date and time values in one of layouts, which describe the
// ISO DateStyle output; time.Parse also accepts fractional seconds after the
// seconds field. BC dates and years past 9999 do not parse.
func timeKey(layouts ...string) func(string) (sortKey, bool) {
	return func(s string) (sortKey, bool) {
		switch s {
		case "-infinity":
			return sortKey{rank: rankNegInf}, true
		case "infinity":
			return sortKey{rank: rankPosInf}, true
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				return sortKey{rank: rankFinite, at: t}, true
			}
		}
		return sortKey{}, false
	}
}
