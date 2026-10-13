package introspect

import (
	"strings"
	"testing"
)

func TestSamplePercentTargetsSampleRows(t *testing.T) {
	for _, tc := range []struct {
		rows int64
		want float64
	}{
		{-1, 100}, // never analyzed: read as far as the LIMIT
		{0, 100},
		{SampleRows, 100},
		{2 * SampleRows, 50},
		{40_000, 25},
		{1_000_000_000, 0.001},
	} {
		if got := samplePercent(tc.rows); got != tc.want {
			t.Errorf("samplePercent(%d) = %v, want %v", tc.rows, got, tc.want)
		}
	}
}

// Names are quoted as identifiers; the percentage and the row cap are
// parameters.
func TestSampleSQLQuotesNames(t *testing.T) {
	got := sampleSQL("my schema", `Mixed "Case"`, []ColumnProfile{{Name: "id"}, {Name: `a"b`}})
	want := `SELECT json_build_object('Rows', count(*), 'Columns', ARRAY[` +
		`ARRAY[count(*) - count("id"), count(DISTINCT "id"::text)], ` +
		`ARRAY[count(*) - count("a""b"), count(DISTINCT "a""b"::text)]])` +
		` FROM (SELECT "id", "a""b" FROM "my schema"."Mixed ""Case""" TABLESAMPLE SYSTEM ($1) LIMIT $2) s`
	if got != want {
		t.Errorf("sampleSQL =\n%s\nwant\n%s", got, want)
	}
}

func TestProfileCells(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{distinct(4), "4"},
		{distinct(1234), "1234"},
		{distinct(-1), "100%"}, // a share of the rows: every value differs
		{distinct(-0.3), "30%"},
		{distinct(0), ""}, // unknown
		{percent(0), "0%"},
		{percent(0.26), "26%"},
		{percent(1.0 / 3), "33.3%"},
		{percent(0.00004), "0.004%"},
		{number(0.001), "0.001"},
		{common([]string{"paid", "it's", ""}, []float64{0.5, 0.25, 0.125}), "'paid' 50%, 'it''s' 25%, '' 12.5%"},
		{common([]string{"a", "b"}, []float64{0.5}), "'a' 50%"},
		{common(nil, nil), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// rows renders the header and rows of p.Result() as tab-separated lines,
// NULL as \N.
func rows(p *TableProfile) []string {
	res := p.Result()
	names := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		names[i] = c.Name
	}
	lines := []string{strings.Join(names, "\t")}
	for _, row := range res.Rows {
		values := make([]string, len(row))
		for i, v := range row {
			values[i] = `\N`
			if v != nil {
				values[i] = *v
			}
		}
		lines = append(lines, strings.Join(values, "\t"))
	}
	return lines
}

func TestProfileRendersStats(t *testing.T) {
	low, high := "1", "50"
	p := &TableProfile{Schema: "app", Name: "orders", Rows: 50, Columns: []ColumnProfile{
		{Name: "id", Type: "bigint", Stats: &ColumnStats{Distinct: -1, Width: 8, Min: &low, Max: &high}},
		{Name: "status", Type: "app.order_status", Stats: &ColumnStats{NullFrac: 0.1, Distinct: 2, Width: 4,
			Common: []string{"paid", "shipped"}, Freqs: []float64{0.6, 0.3}}},
		{Name: "added", Type: "text"}, // no pg_stats row
	}}
	if got, want := p.Summary(), "app.orders: ~50 rows, from pg_stats"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	want := []string{
		"column\ttype\tnulls\tdistinct\twidth\tcommon\tmin\tmax",
		"id\tbigint\t0%\t100%\t8\t\t1\t50",
		"status\tapp.order_status\t10%\t2\t4\t'paid' 60%, 'shipped' 30%\t\t",
		"added\ttext\t\t\t\t\t\t",
	}
	if got := rows(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Result() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	p.Rows, p.More = -1, 3
	if got, want := p.Summary(), "app.orders: from pg_stats; 3 more columns not shown: pass column to profile one"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}

func TestProfileRendersSample(t *testing.T) {
	p := &TableProfile{Schema: "lab", Name: "Mixed Case", Rows: -1, Sampled: true, Sample: 1, Percent: 12.5,
		Columns: []ColumnProfile{{Name: "Weird Col", Type: "integer", Nulls: 1}}}
	want := "lab.Mixed Case: sampled 1 row with TABLESAMPLE SYSTEM (12.5) LIMIT 10000, since pg_stats has nothing " +
		"on these columns (never analyzed, or hidden by row-level security)"
	if got := p.Summary(); got != want {
		t.Errorf("Summary() =\n%s\nwant\n%s", got, want)
	}
	if got := strings.Join(rows(p), "\n"); got != "column\ttype\tnulls\tdistinct\nWeird Col\tinteger\t1\t0" {
		t.Errorf("Result() =\n%s", got)
	}
}
