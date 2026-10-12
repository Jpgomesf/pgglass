package spill

import (
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// table builds rows from strings, reading "" as NULL.
func table(rows ...[]string) [][]*string {
	out := make([][]*string, len(rows))
	for i, row := range rows {
		out[i] = make([]*string, len(row))
		for j, v := range row {
			if v != "" {
				out[i][j] = ptr(v)
			}
		}
	}
	return out
}

func texts(rows [][]*string) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		for _, v := range row {
			out[i] = append(out[i], *v)
		}
	}
	return out
}

func TestColumnStats(t *testing.T) {
	res := &result.Result{
		Columns: []result.Column{
			{Name: "big", TypeOID: pgtype.Int8OID, TypeName: "int8"},
			{Name: "amount", TypeOID: pgtype.NumericOID, TypeName: "numeric"},
			{Name: "ratio", TypeOID: pgtype.Float8OID, TypeName: "float8"},
			{Name: "at", TypeOID: pgtype.TimestamptzOID, TypeName: "timestamptz"},
			{Name: "day", TypeOID: pgtype.DateOID, TypeName: "date"},
			{Name: "old", TypeOID: pgtype.DateOID, TypeName: "date"},
			{Name: "status", TypeOID: 16385},
			{Name: "word", TypeOID: pgtype.TextOID, TypeName: "text"},
		},
		// Values chosen so that comparing text, or int8 as float64, gets min or max wrong.
		Rows: table(
			[]string{"9007199254740992", "1.5", "1e+100", "2026-11-01 01:30:00-04", "2026-01-01", "2026-01-01", "a", "b"},
			[]string{"9007199254740993", "NaN", "-0", "2026-11-01 01:15:00-05", "infinity", "0044-03-15 BC", "b", "a"},
			[]string{"", "-Infinity", "3.5", "2026-10-31 23:59:59.999999-04", "-infinity", "", "a", "zzz"},
			[]string{"-5", "10", "NaN", "2026-11-01 05:30:00+05:30", "1999-12-31", "1999-12-31", "", "a"},
		),
	}
	want := [][]string{
		{"column", "type", "nulls", "distinct", "min", "max"},
		{"big", "int8", "1", "3", "-5", "9007199254740993"},
		{"amount", "numeric", "0", "4", "-Infinity", "NaN"},
		{"ratio", "float8", "0", "4", "-0", "NaN"},
		{"at", "timestamptz", "0", "4", "2026-11-01 05:30:00+05:30", "2026-11-01 01:15:00-05"},
		{"day", "date", "0", "4", "-infinity", "infinity"},
		{"old", "date", "1", "3", "", ""}, // a BC date does not parse: no range rather than a wrong one
		{"status", "oid 16385", "1", "2", "", ""},
		{"word", "text", "0", "3", "", ""}, // text gets no range
	}

	stats := columnStats(res)
	header := make([]string, len(stats.Columns))
	for i, c := range stats.Columns {
		header[i] = c.Name
	}
	got := append([][]string{header}, texts(stats.Rows)...)
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("stats:\ngot  %q\nwant %q", got, want)
	}
}

func TestDistinctCountIsCappedAt1000(t *testing.T) {
	distinct := func(values int, nulls int) string {
		res := &result.Result{Columns: []result.Column{{Name: "v", TypeOID: pgtype.TextOID}}}
		for i := range values {
			res.Rows = append(res.Rows, []*string{ptr(strconv.Itoa(i))}, []*string{ptr(strconv.Itoa(i))})
		}
		for range nulls {
			res.Rows = append(res.Rows, []*string{nil})
		}
		return *columnStats(res).Rows[0][3]
	}
	tests := []struct {
		values, nulls int
		want          string
	}{
		{0, 3, "0"},
		{1, 3, "1"},
		{999, 0, "999"},
		{1000, 5, "1000"},
		{1001, 0, ">1000"},
		{5000, 0, ">1000"},
	}
	for _, tt := range tests {
		if got := distinct(tt.values, tt.nulls); got != tt.want {
			t.Errorf("%d distinct values and %d NULLs: distinct = %s, want %s", tt.values, tt.nulls, got, tt.want)
		}
	}
}

// assertAscending checks that key orders values strictly ascending.
func assertAscending(t *testing.T, key func(string) (sortKey, bool), values []string) {
	t.Helper()
	var prev sortKey
	prevValue := ""
	for i, v := range values {
		k, ok := key(v)
		if !ok {
			t.Fatalf("%q does not parse", v)
		}
		if i > 0 && prev.compare(k) >= 0 {
			t.Errorf("%q does not order before %q", prevValue, v)
		}
		if k.compare(k) != 0 {
			t.Errorf("%q does not equal itself", v)
		}
		prev, prevValue = k, v
	}
}

func TestNumbersOrderLikePostgres(t *testing.T) {
	assertAscending(t, numberKey, []string{
		"-Infinity", "-1e+100", "-9223372036854775808", "-1", "-0.5", "0", "1e-300", "0.5", "1",
		"9007199254740992", "9007199254740993", "123456789012345678901234567890.000000000000000001", "1e+100",
		"Infinity", "NaN",
	})
	zero, _ := numberKey("0")
	negZero, _ := numberKey("-0")
	scaled, _ := numberKey("0.000")
	if zero.compare(negZero) != 0 || zero.compare(scaled) != 0 {
		t.Error("0, -0 and 0.000 must compare equal")
	}
	if _, ok := numberKey("12abc"); ok {
		t.Error("a malformed number parsed")
	}
}

func TestTimesOrderByInstant(t *testing.T) {
	assertAscending(t, keyFor(pgtype.TimestamptzOID), []string{
		"-infinity",
		"0001-01-01 00:00:00+00",
		"1900-01-01 00:00:00-04:56:02", // a local mean time offset
		"2026-11-01 05:30:00+05:30",    // 00:00 UTC
		"2026-11-01 01:30:00-04",       // 05:30 UTC
		"2026-11-01 01:15:00-05",       // 06:15 UTC, though it sorts first as text
		"2026-11-01 01:15:00.000001-05",
		"9999-12-31 23:59:59.999999+00",
		"infinity",
	})
	assertAscending(t, keyFor(pgtype.TimestampOID), []string{"-infinity", "1999-12-31 23:59:59.5", "2000-01-01 00:00:00", "infinity"})
	assertAscending(t, keyFor(pgtype.DateOID), []string{"-infinity", "0001-01-01", "1999-12-31", "2000-01-01", "infinity"})
	assertAscending(t, keyFor(pgtype.TimeOID), []string{"00:00:00", "09:59:59.999999", "10:00:00", "23:59:59"})
	assertAscending(t, keyFor(pgtype.TimetzOID), []string{"10:00:00+02", "09:30:00+00", "10:00:00-01:00"})
}

func TestTimesThatDoNotParse(t *testing.T) {
	tests := []struct {
		oid uint32
		v   string
	}{
		{pgtype.DateOID, "0044-03-15 BC"},
		{pgtype.DateOID, "10000-01-01"},
		{pgtype.DateOID, "12/10/2026"}, // a non-ISO DateStyle
		{pgtype.TimestampOID, "2026-10-12 10:15:30 BC"},
		{pgtype.TimestamptzOID, "2026-10-12 10:15:30"},
		{pgtype.TimeOID, "24:00:00"},
	}
	for _, tt := range tests {
		if _, ok := keyFor(tt.oid)(tt.v); ok {
			t.Errorf("oid %d: %q parsed", tt.oid, tt.v)
		}
	}
}

func TestOnlyNumericAndTemporalTypesGetARange(t *testing.T) {
	for _, oid := range []uint32{pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.Float4OID, pgtype.Float8OID,
		pgtype.NumericOID, pgtype.DateOID, pgtype.TimeOID, pgtype.TimetzOID, pgtype.TimestampOID, pgtype.TimestamptzOID} {
		if keyFor(oid) == nil {
			t.Errorf("oid %d gets no min and max", oid)
		}
	}
	for _, oid := range []uint32{pgtype.TextOID, pgtype.BoolOID, pgtype.UUIDOID, pgtype.JSONBOID, pgtype.IntervalOID,
		pgtype.Int4ArrayOID, 16385} {
		if keyFor(oid) != nil {
			t.Errorf("oid %d gets a min and max", oid)
		}
	}
}
