package introspect

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/result"
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
	Sampled bool
	Sample  int64
	Percent float64
	More    int
	Columns []ColumnProfile
}

// ColumnProfile describes the values of one column.
type ColumnProfile struct {
	Name  string
	Type  string
	Stats *ColumnStats
	// Nulls and Distinct count the nulls and the distinct values of the
	// column in the sample, when the profile is Sampled.
	Nulls    int64
	Distinct int64
}

// ColumnStats is the pg_stats row of a column, as of the last ANALYZE.
type ColumnStats struct {
	NullFrac float64
	// Distinct is n_distinct: the number of distinct non-null values or, when
	// negative, minus that number divided by the rows; 0 when unknown.
	Distinct float64
	Width    int
	Common   []string
	Freqs    []float64
	Min, Max *string
}

const profiled = `a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
	AND has_column_privilege(c.oid, a.attnum, 'SELECT') AND ($3 = '' OR a.attname = $3)`

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

// Profile describes up to ProfileColumns readable columns of schema.name, or
// column alone, from pg_stats or else a sample of at most SampleRows rows.
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

func (p *TableProfile) sample(ctx context.Context, q engine.Querier) error {
	var counts struct {
		Rows    int64
		Columns [][2]int64
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

func samplePercent(rows int64) float64 {
	if rows <= SampleRows {
		return 100
	}
	return 100 * SampleRows / float64(rows)
}

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
			"(never analyzed, hidden by row-level security, or virtual generated columns, which ANALYZE skips)", count(p.Sample, "row"), number(p.Percent), SampleRows)
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

// Result returns one row per column: its type, nulls and distinct values, plus,
// from pg_stats, average width, most common values and histogram bounds.
func (p *TableProfile) Result() *result.Result {
	if p.Sampled {
		res := result.Text("column", "type", "nulls", "distinct")
		for _, c := range p.Columns {
			res.AddRow(c.Name, c.Type, strconv.FormatInt(c.Nulls, 10), strconv.FormatInt(c.Distinct, 10))
		}
		return res
	}
	res := result.Text("column", "type", "nulls", "distinct", "width", "common", "min", "max")
	for _, c := range p.Columns {
		res.AddRow(append([]string{c.Name, c.Type}, c.Stats.cells()...)...)
	}
	return res
}

func (s *ColumnStats) cells() []string {
	if s == nil {
		return make([]string, 6)
	}
	return []string{percent(s.NullFrac), distinct(s.Distinct), strconv.Itoa(s.Width), common(s.Common, s.Freqs), deref(s.Min), deref(s.Max)}
}

func distinct(n float64) string {
	switch {
	case n > 0:
		return strconv.FormatFloat(n, 'f', 0, 64)
	case n < 0:
		return percent(-n)
	default:
		return ""
	}
}

func common(values []string, freqs []float64) string {
	parts := make([]string, 0, len(values))
	for i, f := range freqs {
		if i < len(values) {
			parts = append(parts, quote(values[i])+" "+percent(f))
		}
	}
	return strings.Join(parts, ", ")
}

func percent(f float64) string {
	return number(f*100) + "%"
}

func number(f float64) string {
	return strconv.FormatFloat(f, 'g', 3, 64)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
