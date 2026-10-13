package introspect

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

const (
	// ProfileColumns is the most columns Profile describes; it counts the rest.
	ProfileColumns = 50
	// SampleRows is the most rows Profile samples from a table that pg_stats
	// has nothing on.
	SampleRows = 10_000
)

// TableProfile describes the values in the columns of a table.
type TableProfile struct {
	Schema string
	Name   string
	// Rows is the planner's row estimate (pg_class.reltuples, summed over the
	// partitions of a partitioned table); -1 when unknown.
	Rows    int64
	Sampled bool    // pg_stats has nothing on the columns: they come from a sample
	Sample  int64   // rows in the sample, when Sampled
	Percent float64 // the TABLESAMPLE SYSTEM percentage of the sample, when Sampled
	More    int     // columns the role can SELECT past the first ProfileColumns
	Columns []ColumnProfile
}

// ColumnProfile describes the values of one column.
type ColumnProfile struct {
	Name  string
	Type  string       // format_type, e.g. "numeric(12,2)"
	Stats *ColumnStats // its pg_stats row; nil when there is none
	// Nulls and Distinct count the nulls and the distinct values of the
	// column in the sample, when the profile is Sampled.
	Nulls    int64
	Distinct int64
}

// ColumnStats is the pg_stats row of a column, as of the last ANALYZE.
type ColumnStats struct {
	NullFrac float64 // the fraction of rows that are null
	// Distinct is n_distinct: the number of distinct non-null values or, when
	// negative, minus that number divided by the rows; 0 when unknown.
	Distinct float64
	Width    int       // average width in bytes
	Common   []string  // the 5 most common values, most common first
	Freqs    []float64 // the fraction of rows each of Common takes
	Min, Max *string   // the first and last histogram bounds; nil without a histogram
}

// profiled keeps pg_attribute a to the columns of pg_class c that the role
// can SELECT, or to column $3 alone when $3 is not empty.
const profiled = `a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
	AND has_column_privilege(c.oid, a.attnum, 'SELECT') AND ($3 = '' OR a.attname = $3)`

// columnStatsJSON is the ColumnStats of column a of pg_class c in
// pg_namespace n, or NULL. pg_stats shows only the columns the role can
// SELECT, and none while row-level security applies to it. A table with
// inheritance children has a row over the whole hierarchy too, which wins.
// anyarray cannot be sliced, hence the casts to text[].
const columnStatsJSON = `(SELECT json_build_object(
		'NullFrac', s.null_frac,
		'Distinct', s.n_distinct,
		'Width', s.avg_width,
		'Common', (s.most_common_vals::text::text[])[1:5],
		'Freqs', s.most_common_freqs[1:5],
		'Min', (s.histogram_bounds::text::text[])[1],
		'Max', (s.histogram_bounds::text::text[])[array_length(s.histogram_bounds, 1)])
	FROM pg_stats s
	WHERE s.schemaname = n.nspname AND s.tablename = c.relname AND s.attname = a.attname
	ORDER BY s.inherited DESC
	LIMIT 1)`

// profileSQL returns no row when relation $1.$2 is not a table, partitioned
// table or materialized view. Columns holds the first $4 profiled columns.
// A partitioned table has no rows of its own: its partitions' are summed.
const profileSQL = `
SELECT json_build_object(
	'Rows', CASE WHEN c.relkind = 'p'
		THEN coalesce((SELECT sum(greatest(r.reltuples, 0)) FROM pg_partition_tree(c.oid) t
			JOIN pg_class r ON r.oid = t.relid WHERE t.isleaf), 0)
		ELSE c.reltuples END::bigint,
	'Readable', (SELECT count(*) FROM pg_attribute a WHERE ` + profiled + `),
	'Columns', (SELECT json_agg(json_build_object(
			'Name', a.attname,
			'Type', format_type(a.atttypid, a.atttypmod),
			'Stats', ` + columnStatsJSON + `)
		ORDER BY a.attnum)
		FROM (SELECT a.attnum, a.attname, a.atttypid, a.atttypmod FROM pg_attribute a
			WHERE ` + profiled + ` ORDER BY a.attnum LIMIT $4) a))
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'm')`

// Profile describes the values of the columns of table schema.name that the
// current role can SELECT, the first ProfileColumns of them, or of column
// alone when it is not empty. Names match exactly, as the catalog stores them.
//
// The figures come from pg_stats, which reads nothing from the table. When
// it has nothing on any of the columns (the table was never analyzed, or
// row-level security hides its statistics), Profile reads a sample of the
// table instead, at most SampleRows rows taken with TABLESAMPLE SYSTEM, as
// the role: privileges and row-level security apply.
func Profile(ctx context.Context, q engine.Querier, schema, name, column string) (*TableProfile, error) {
	var found struct {
		Rows     int64
		Readable int
		Columns  []ColumnProfile
	}
	err := queryJSON(ctx, q, &found, profileSQL, schema, name, column, ProfileColumns)
	if errors.Is(err, errNoRows) {
		return nil, fmt.Errorf("relation %q.%q does not exist or is not a table or materialized view", schema, name)
	}
	if err != nil {
		return nil, fmt.Errorf("profile relation %q.%q: %w", schema, name, err)
	}
	if len(found.Columns) == 0 {
		return nil, noColumns(schema, name, column)
	}
	p := &TableProfile{Schema: schema, Name: name, Rows: found.Rows, More: found.Readable - len(found.Columns), Columns: found.Columns}
	if p.hasStats() {
		return p, nil
	}
	if err := p.sample(ctx, q); err != nil {
		return nil, fmt.Errorf("sample relation %q.%q: %w", schema, name, err)
	}
	return p, nil
}

// noColumns explains a profile with no columns: pg_stats shows only the
// columns the role can SELECT.
func noColumns(schema, name, column string) error {
	if column != "" {
		return fmt.Errorf("column %q of %q.%q does not exist or the current role cannot SELECT it: "+
			"pg_stats shows only the columns a role can SELECT", column, schema, name)
	}
	return fmt.Errorf("the current role can SELECT no column of %q.%q: pg_stats shows only the columns a role can SELECT", schema, name)
}

func (p *TableProfile) hasStats() bool {
	for _, c := range p.Columns {
		if c.Stats != nil {
			return true
		}
	}
	return false
}

// sample counts the rows of a sample of the table, then the nulls and the
// distinct values of each column in it.
func (p *TableProfile) sample(ctx context.Context, q engine.Querier) error {
	var counts struct {
		Rows    int64
		Columns [][2]int64 // nulls and distinct values, per column
	}
	percent := samplePercent(p.Rows)
	if err := queryJSON(ctx, q, &counts, sampleSQL(p.Schema, p.Name, p.Columns), percent, SampleRows); err != nil {
		return err
	}
	if len(counts.Columns) != len(p.Columns) {
		return fmt.Errorf("sample counted %d columns, want %d", len(counts.Columns), len(p.Columns))
	}
	p.Sampled, p.Sample, p.Percent = true, counts.Rows, percent
	for i, c := range counts.Columns {
		p.Columns[i].Nulls, p.Columns[i].Distinct = c[0], c[1]
	}
	return nil
}

// samplePercent returns the TABLESAMPLE SYSTEM percentage that reads about
// SampleRows of a table of rows rows: all of a smaller table, and of one whose
// rows are unknown (-1), as much as the LIMIT allows.
func samplePercent(rows int64) float64 {
	if rows <= SampleRows {
		return 100
	}
	return 100 * SampleRows / float64(rows)
}

// sampleSQL counts, in a sample of at most $2 rows of table schema.name taken
// with TABLESAMPLE SYSTEM ($1), the rows, then the nulls and the distinct
// values of each of columns. Values are compared as text, since some types,
// such as json, have no equality operator.
func sampleSQL(schema, name string, columns []ColumnProfile) string {
	names := make([]string, len(columns))
	counts := make([]string, len(columns))
	for i, c := range columns {
		col := pgx.Identifier{c.Name}.Sanitize()
		names[i] = col
		counts[i] = "ARRAY[count(*) - count(" + col + "), count(DISTINCT " + col + "::text)]"
	}
	return "SELECT json_build_object('Rows', count(*), 'Columns', ARRAY[" + strings.Join(counts, ", ") + "])" +
		" FROM (SELECT " + strings.Join(names, ", ") + " FROM " + pgx.Identifier{schema, name}.Sanitize() +
		" TABLESAMPLE SYSTEM ($1) LIMIT $2) s"
}

// Summary is the line that introduces Result: the table, its estimated rows,
// where the figures come from, and how many columns are left out.
func (p *TableProfile) Summary() string {
	s := p.Schema + "." + p.Name + ": "
	switch {
	case p.Sampled:
		s += fmt.Sprintf("sampled %s with TABLESAMPLE SYSTEM (%s) LIMIT %d, since pg_stats has nothing on these columns "+
			"(never analyzed, or hidden by row-level security)", count(p.Sample, "row"), number(p.Percent), SampleRows)
	case p.Rows >= 0:
		s += "~" + count(p.Rows, "row") + ", from pg_stats"
	default:
		s += "from pg_stats"
	}
	if p.More > 0 {
		s += fmt.Sprintf("; %d more columns not shown: pass column to profile one", p.More)
	}
	return s
}

// Result returns one row per column. From pg_stats: its type, the share of
// nulls, distinct values, average width in bytes, most common values and the
// histogram's bounds. From a sample: its type, and the nulls and distinct
// values in the sample.
func (p *TableProfile) Result() *result.Result {
	if p.Sampled {
		res := textResult("column", "type", "nulls", "distinct")
		for _, c := range p.Columns {
			addRow(res, c.Name, c.Type, strconv.FormatInt(c.Nulls, 10), strconv.FormatInt(c.Distinct, 10))
		}
		return res
	}
	res := textResult("column", "type", "nulls", "distinct", "width", "common", "min", "max")
	for _, c := range p.Columns {
		addRow(res, append([]string{c.Name, c.Type}, c.Stats.cells()...)...)
	}
	return res
}

// cells renders s as the nulls, distinct, width, common, min and max cells
// of Result, all empty when s is nil.
func (s *ColumnStats) cells() []string {
	if s == nil {
		return make([]string, 6)
	}
	return []string{percent(s.NullFrac), distinct(s.Distinct), strconv.Itoa(s.Width), common(s.Common, s.Freqs), deref(s.Min), deref(s.Max)}
}

// distinct renders n_distinct: a count, or a share of the rows when negative.
func distinct(n float64) string {
	switch {
	case n > 0:
		return strconv.FormatFloat(n, 'f', 0, 64)
	case n < 0:
		return percent(-n)
	default:
		return "" // unknown
	}
}

// common renders the most common values as SQL literals, each with its
// share of the rows, e.g. "'paid' 26%, 'shipped' 24%".
func common(values []string, freqs []float64) string {
	parts := make([]string, 0, len(values))
	for i, f := range freqs {
		if i < len(values) {
			parts = append(parts, quote(values[i])+" "+percent(f))
		}
	}
	return strings.Join(parts, ", ")
}

// percent renders a fraction as a percentage, e.g. 0.26 as "26%".
func percent(f float64) string {
	return number(f*100) + "%"
}

// number renders f with up to 3 significant digits.
func number(f float64) string {
	return strconv.FormatFloat(f, 'g', 3, 64)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textResult returns an empty result whose columns, named names, hold text.
func textResult(names ...string) *result.Result {
	cols := make([]result.Column, len(names))
	for i, name := range names {
		cols[i] = result.Column{Name: name, TypeOID: pgtype.TextOID, TypeName: "text"}
	}
	return &result.Result{Columns: cols}
}

func addRow(res *result.Result, cells ...string) {
	row := make([]*string, len(cells))
	for i := range cells {
		row[i] = &cells[i]
	}
	res.Rows = append(res.Rows, row)
}
