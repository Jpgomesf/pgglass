package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/result"
)

func TestSetupAppliesLimitsAsLocalSettings(t *testing.T) {
	l := limits{StatementTimeout: 30 * time.Second, LockTimeout: 1500 * time.Millisecond}

	sql, args := setup(l, "")
	want := "SELECT pg_catalog.set_config('statement_timeout', $1, true)," +
		" pg_catalog.set_config('lock_timeout', $2, true)," +
		" pg_catalog.set_config('idle_in_transaction_session_timeout', $3, true)," +
		" pg_catalog.set_config('standard_conforming_strings', 'on', true)"
	if sql != want {
		t.Errorf("sql = %q\nwant  %q", sql, want)
	}
	if want := []any{"30000", "1500", "30000"}; !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestSetupQuotesSearchPath(t *testing.T) {
	sql, args := setup(limits{StatementTimeout: time.Second, LockTimeout: time.Second}, `app"; RESET ALL; --`)
	if want := ", pg_catalog.set_config('search_path', $4, true)"; len(sql) < len(want) || sql[len(sql)-len(want):] != want {
		t.Errorf("sql = %q, want it to end with %q", sql, want)
	}
	if want := []any{"1000", "1000", "1000", `"app""; RESET ALL; --"`}; !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestLimitsForOnlyLowersMaxRows(t *testing.T) {
	m := &Manager{limits: limits{MaxRows: 100, MaxBytes: 7}}
	for _, tc := range []struct{ asked, want int }{
		{0, 100},
		{-5, 100},
		{1, 1},
		{99, 99},
		{100, 100},
		{101, 100},
	} {
		got := m.limitsFor(Query{MaxRows: tc.asked})
		if got.MaxRows != tc.want || got.MaxBytes != 7 {
			t.Errorf("limitsFor(MaxRows %d) = %+v, want MaxRows %d and MaxBytes 7", tc.asked, got, tc.want)
		}
	}
	if m.limits.MaxRows != 100 {
		t.Errorf("limitsFor changed the Manager's limits: %+v", m.limits)
	}
}

// A new connection may take another password or TLS settings, never another
// user, database or host.
func TestTargetIsUserDatabaseAndHosts(t *testing.T) {
	parse := func(dsn string) string {
		t.Helper()
		c, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", dsn, err)
		}
		return target(c)
	}
	first := parse("postgres://u:old@a:5432,b:5433/d")
	for dsn, same := range map[string]bool{
		"postgres://u:new@a:5432,b:5433/d":                 true,
		"postgres://u:old@a:5432,b:5433/d?sslmode=require": true,
		"postgres://u:old@b:5433,a:5432/d":                 true,
		"postgres://v:old@a:5432,b:5433/d":                 false,
		"postgres://u:old@a:5432,b:5433/e":                 false,
		"postgres://u:old@a:5432,c:5433/d":                 false,
		"postgres://u:old@a:5432,b:5434/d":                 false,
		"postgres://u:old@a:5432/d":                        false,
	} {
		if got := parse(dsn) == first; got != same {
			t.Errorf("%s: same target = %t, want %t", dsn, got, same)
		}
	}
}

func rawRow(values ...string) [][]byte {
	row := make([][]byte, len(values))
	for i, v := range values {
		if v != "\x00" {
			row[i] = []byte(v)
		}
	}
	return row
}

func collectAll(l limits, rows ...[][]byte) (*result.Result, int) {
	c := collector{limits: l, res: &result.Result{}}
	kept := 0
	for _, r := range rows {
		if !c.add(r) {
			break
		}
		kept++
	}
	return c.res, kept
}

func TestCollectorLimits(t *testing.T) {
	row := rawRow("ab", "cd")
	for _, tc := range []struct {
		name      string
		limits    limits
		rows      int
		wantKept  int
		wantTrunc string
	}{
		{"under both limits", limits{MaxRows: 5, MaxBytes: 1000}, 3, 3, ""},
		{"exactly max_rows", limits{MaxRows: 3, MaxBytes: 1000}, 3, 3, ""},
		{"one past max_rows", limits{MaxRows: 3, MaxBytes: 1000}, 4, 3, "max_rows=3"},
		{"exactly max_bytes", limits{MaxRows: 100, MaxBytes: 156}, 3, 3, ""},
		{"one byte short", limits{MaxRows: 100, MaxBytes: 155}, 3, 2, "max_bytes=155"},
		{"first row too big", limits{MaxRows: 100, MaxBytes: 51}, 1, 0, "max_bytes=51"},
		{"max_rows checked first", limits{MaxRows: 1, MaxBytes: 52}, 2, 1, "max_rows=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([][][]byte, tc.rows)
			for i := range rows {
				rows[i] = row
			}
			res, kept := collectAll(tc.limits, rows...)
			if kept != tc.wantKept || len(res.Rows) != tc.wantKept {
				t.Errorf("kept %d rows (%d in result), want %d", kept, len(res.Rows), tc.wantKept)
			}
			if res.Truncated != (tc.wantTrunc != "") || res.TruncatedReason != tc.wantTrunc {
				t.Errorf("Truncated = %v %q, want %q", res.Truncated, res.TruncatedReason, tc.wantTrunc)
			}
		})
	}
}

// Empty and NULL values take memory too: 1,600 empty columns once weighed
// nothing, so 1,000 such rows fit any byte budget.
func TestCollectorCountsEmptyAndNullValues(t *testing.T) {
	empty := make([][]byte, 1600)
	for i := range empty {
		if i%2 == 0 {
			empty[i] = []byte{}
		}
	}
	rows := make([][][]byte, 1000)
	for i := range rows {
		rows[i] = empty
	}
	res, kept := collectAll(limits{MaxRows: 1000, MaxBytes: 1 << 20}, rows...)
	if want := (1 << 20) / (1600 * valueCost); kept != want || res.TruncatedReason != "max_bytes=1048576" {
		t.Errorf("kept %d rows, truncated %q; want %d, max_bytes=1048576", kept, res.TruncatedReason, want)
	}
}

func TestCollectorKeepsNullDistinctAndCopiesValues(t *testing.T) {
	raw := rawRow("\x00", "", "abc")
	res, _ := collectAll(limits{MaxRows: 10, MaxBytes: 3 + 3*valueCost}, raw)
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %v, want one", res.Rows)
	}
	row := res.Rows[0]
	if row[0] != nil {
		t.Errorf("NULL became %q", *row[0])
	}
	if row[1] == nil || *row[1] != "" {
		t.Errorf("empty string became %v", row[1])
	}

	raw[2][0] = 'X'
	if *row[2] != "abc" {
		t.Errorf("kept value changed with the buffer: %q", *row[2])
	}
}

func TestColumnsNamesTypesPgxKnows(t *testing.T) {
	fields := []pgconn.FieldDescription{
		{Name: "a", DataTypeOID: pgtype.Int4OID},
		{Name: "a", DataTypeOID: pgtype.NumericOID},
		{Name: "tags", DataTypeOID: pgtype.TextArrayOID},
		{Name: "status", DataTypeOID: 16_500},
	}
	want := []result.Column{
		{Name: "a", TypeOID: pgtype.Int4OID, TypeName: "int4"},
		{Name: "a", TypeOID: pgtype.NumericOID, TypeName: "numeric"},
		{Name: "tags", TypeOID: pgtype.TextArrayOID, TypeName: "_text"},
		{Name: "status", TypeOID: 16_500, TypeName: ""},
	}
	if got := columns(pgtype.NewMap(), fields); !slices.Equal(got, want) {
		t.Errorf("columns = %+v\nwant      %+v", got, want)
	}
}

func TestQueryErrorFlattensPostgresErrors(t *testing.T) {
	pgErr := &pgconn.PgError{
		Severity: "ERROR", Code: "22P02", Message: "invalid input syntax for type json",
		Detail: `Token "x" is invalid.`, Hint: "a hint", Position: 8,
	}
	want := &QueryError{SQLState: "22P02", Message: pgErr.Message, Detail: pgErr.Detail, Hint: "a hint", Position: 8}

	for _, err := range []error{pgErr, fmt.Errorf("wrapped: %w", pgErr)} {
		var got *QueryError
		if !errors.As(queryError(context.Background(), err), &got) || *got != *want {
			t.Errorf("queryError(%v) = %+v, want %+v", err, got, want)
		}
	}
}

func TestQueryErrorReportsEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := queryError(ctx, &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"})
	var qe *QueryError
	if !errors.Is(err, context.Canceled) || errors.As(err, &qe) {
		t.Errorf("err = %v, want a context error, not a *QueryError", err)
	}
}

func TestQueryErrorKeepsOtherErrors(t *testing.T) {
	other := errors.New("dial tcp: connection refused")
	if err := queryError(context.Background(), other); err != other {
		t.Errorf("queryError = %v, want the error unchanged", err)
	}
}
