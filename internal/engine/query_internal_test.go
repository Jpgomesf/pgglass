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

	"github.com/jpgomesf/mcp-postgres/internal/result"
)

func TestSetupAppliesLimitsAsLocalSettings(t *testing.T) {
	l := Limits{StatementTimeout: 30 * time.Second, LockTimeout: 1500 * time.Millisecond}

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
	sql, args := setup(Limits{StatementTimeout: time.Second, LockTimeout: time.Second}, `app"; RESET ALL; --`)
	if want := ", pg_catalog.set_config('search_path', $4, true)"; len(sql) < len(want) || sql[len(sql)-len(want):] != want {
		t.Errorf("sql = %q, want it to end with %q", sql, want)
	}
	if want := []any{"1000", "1000", "1000", `"app""; RESET ALL; --"`}; !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestLimitsForOnlyLowersMaxRows(t *testing.T) {
	m := &Manager{limits: Limits{MaxRows: 100, MaxBytes: 7}}
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

// rawRow builds a row as pgx returns it; "\x00" stands for NULL.
func rawRow(values ...string) [][]byte {
	row := make([][]byte, len(values))
	for i, v := range values {
		if v != "\x00" {
			row[i] = []byte(v)
		}
	}
	return row
}

func collectAll(limits Limits, rows ...[][]byte) (*result.Result, int) {
	c := collector{limits: limits, res: &result.Result{}}
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
	four := rawRow("ab", "cd") // 4 bytes
	for _, tc := range []struct {
		name      string
		limits    Limits
		rows      int
		wantKept  int
		wantTrunc string
	}{
		{"under both limits", Limits{MaxRows: 5, MaxBytes: 100}, 3, 3, ""},
		{"exactly max_rows", Limits{MaxRows: 3, MaxBytes: 100}, 3, 3, ""},
		{"one past max_rows", Limits{MaxRows: 3, MaxBytes: 100}, 4, 3, "max_rows=3"},
		{"exactly max_bytes", Limits{MaxRows: 100, MaxBytes: 12}, 3, 3, ""},
		{"one byte short", Limits{MaxRows: 100, MaxBytes: 11}, 3, 2, "max_bytes=11"},
		{"first row too big", Limits{MaxRows: 100, MaxBytes: 3}, 1, 0, "max_bytes=3"},
		{"max_rows checked first", Limits{MaxRows: 1, MaxBytes: 4}, 2, 1, "max_rows=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([][][]byte, tc.rows)
			for i := range rows {
				rows[i] = four
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

func TestCollectorKeepsNullDistinctAndCopiesValues(t *testing.T) {
	raw := rawRow("\x00", "", "abc")
	res, _ := collectAll(Limits{MaxRows: 10, MaxBytes: 3}, raw) // NULL and "" weigh nothing
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

	raw[2][0] = 'X' // pgx reuses its buffer for the next row
	if *row[2] != "abc" {
		t.Errorf("kept value changed with the buffer: %q", *row[2])
	}
}

func TestColumnsNamesTypesPgxKnows(t *testing.T) {
	fields := []pgconn.FieldDescription{
		{Name: "a", DataTypeOID: pgtype.Int4OID},
		{Name: "a", DataTypeOID: pgtype.NumericOID},
		{Name: "tags", DataTypeOID: pgtype.TextArrayOID},
		{Name: "status", DataTypeOID: 16_500}, // an enum: not built in
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
	// Postgres answers the cancel request pgx sends with 57014.
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
