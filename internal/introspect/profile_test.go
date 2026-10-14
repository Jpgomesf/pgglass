package introspect_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const profileDB = "introspect_profile"

//go:embed testdata/profile.sql
var profileFixture string

func profile(t *testing.T, q engine.Querier, schema, name, column string) *introspect.TableProfile {
	t.Helper()
	p, err := introspect.Profile(t.Context(), q, schema, name, column)
	if err != nil {
		t.Fatalf("Profile(%s.%s, column %q): %v", schema, name, column, err)
	}
	return p
}

func columns(p *introspect.TableProfile) map[string]introspect.ColumnProfile {
	byName := make(map[string]introspect.ColumnProfile, len(p.Columns))
	for _, c := range p.Columns {
		byName[c.Name] = c
	}
	return byName
}

func ptrTo(s string) *string { return &s }

func equalColumns(t *testing.T, what string, got, want []introspect.ColumnProfile) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", what, showColumns(got), showColumns(want))
	}
}

func showColumns(cols []introspect.ColumnProfile) string {
	var s string
	for _, c := range cols {
		s += fmt.Sprintf("\n  %s %s nulls=%d distinct=%d", c.Name, c.Type, c.Nulls, c.Distinct)
		if st := c.Stats; st != nil {
			s += fmt.Sprintf(" stats=%+v", *st)
			if st.Min != nil && st.Max != nil {
				s += fmt.Sprintf(" min=%q max=%q", *st.Min, *st.Max)
			}
		}
	}
	return s
}

// The app fixture is analyzed, and every row is in ANALYZE's sample, so its
// statistics are exact.
func TestProfileReadsPgStats(t *testing.T) {
	p := profile(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "orders", "")
	if p.Schema != "app" || p.Name != "orders" || p.Rows != 50 || p.Sampled || p.More != 0 || len(p.Columns) != 9 {
		t.Fatalf("profile of app.orders = %+v", p)
	}
	got := columns(p)
	equalColumns(t, "app.orders", []introspect.ColumnProfile{got["status"], got["total"], got["metadata"]}, []introspect.ColumnProfile{
		{Name: "status", Type: "app.order_status", Stats: &introspect.ColumnStats{Distinct: 4, Width: 4,
			Common: []string{"paid", "shipped", "pending", "cancelled"}, Freqs: []float64{0.26, 0.26, 0.24, 0.24}}},
		{Name: "total", Type: "numeric(12,2)", Stats: &introspect.ColumnStats{Distinct: -1, Width: 6,
			Min: ptrTo("12.34"), Max: ptrTo("617.00")}},
		{Name: "metadata", Type: "jsonb", Stats: &introspect.ColumnStats{NullFrac: 0.16, Distinct: 4, Width: 45,
			Common: []string{`{"items": 2, "channel": "store"}`, `{"items": 4, "channel": "store"}`, `{"items": 3, "channel": "web"}`, `{"items": 1, "channel": "web"}`},
			Freqs:  []float64{0.26, 0.24, 0.18, 0.16}}},
	})
	if q := got["quantities"].Stats; q == nil || q.Distinct != -0.3 || len(q.Common) != 5 || len(q.Freqs) != 5 || q.Common[0] != "{1,4}" {
		t.Errorf("quantities = %+v", q)
	}

	single := profile(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "orders", "status")
	equalColumns(t, "status alone", single.Columns, []introspect.ColumnProfile{got["status"]})
}

func loadProfile(t *testing.T) engine.Querier {
	t.Helper()
	testdb.CreateDatabase(t, profileDB, profileFixture)
	return testdb.Querier(t, testdb.RoleClean, profileDB)
}

func TestProfileSamplesATableNeverAnalyzed(t *testing.T) {
	clean := loadProfile(t)
	p := profile(t, clean, "public", "fresh", "")
	equal(t, "fresh", *p, introspect.TableProfile{Schema: "public", Name: "fresh", Rows: -1, Sampled: true, Sample: 300, Percent: 100,
		Columns: []introspect.ColumnProfile{
			{Name: "id", Type: "integer", Distinct: 300},
			{Name: "note", Type: "text", Nulls: 100, Distinct: 7},
			{Name: "doc", Type: "json", Distinct: 2},
		}})
	single := profile(t, clean, "public", "fresh", "note")
	equalColumns(t, "fresh.note", single.Columns, []introspect.ColumnProfile{{Name: "note", Type: "text", Nulls: 100, Distinct: 7}})
}

// The parent of the partitions has no statistics: the sample takes the same
// share of each partition, enough for SampleRows of their rows.
func TestProfileSamplesAPartitionedTableByItsPartitionsRows(t *testing.T) {
	p := profile(t, loadProfile(t), "public", "events", "")
	if p.Rows != 40_000 || !p.Sampled || p.Percent != 25 || p.Sample <= 0 || p.Sample > introspect.SampleRows {
		t.Fatalf("profile of events = %+v", p)
	}
	got := columns(p)
	if id, part := got["id"], got["part"]; id.Distinct != p.Sample || id.Nulls != 0 || part.Distinct != 2 {
		t.Errorf("id = %+v, part = %+v, in a sample of %d rows", id, part, p.Sample)
	}
}

func TestProfileShowsOnlyTheColumnsTheRoleCanSelect(t *testing.T) {
	p := profile(t, loadProfile(t), "public", "secrets", "")
	if p.Sampled || p.Rows != 30 {
		t.Fatalf("profile of secrets = %+v", p)
	}
	equalColumns(t, "secrets", p.Columns, []introspect.ColumnProfile{
		{Name: "id", Type: "integer", Stats: &introspect.ColumnStats{Distinct: -1, Width: 4, Min: ptrTo("1"), Max: ptrTo("30")}},
		{Name: "label", Type: "text", Stats: &introspect.ColumnStats{Distinct: 3, Width: 8,
			Common: []string{"label 0", "label 1", "label 2"}, Freqs: []float64{0.33333334, 0.33333334, 0.33333334}}},
	})
	if all := profile(t, testdb.Querier(t, testdb.Superuser, profileDB), "public", "secrets", ""); len(all.Columns) != 3 {
		t.Errorf("the superuser sees %d columns, want 3", len(all.Columns))
	}
}

func TestProfileShowsTheFirstColumnsAndCountsTheRest(t *testing.T) {
	clean := loadProfile(t)
	p := profile(t, clean, "public", "wide", "")
	if len(p.Columns) != introspect.ProfileColumns || p.More != 10 || p.Columns[0].Name != "c01" || p.Columns[49].Name != "c50" {
		t.Fatalf("profile of wide: %d columns, %d more, %+v", len(p.Columns), p.More, p.Columns)
	}
	last := profile(t, clean, "public", "wide", "c60")
	equalColumns(t, "wide.c60", last.Columns, []introspect.ColumnProfile{
		{Name: "c60", Type: "integer", Stats: &introspect.ColumnStats{NullFrac: 1, Width: 4}},
	})
	if last.More != 0 {
		t.Errorf("one column, %d more", last.More)
	}
}

func TestProfileErrors(t *testing.T) {
	clean := loadProfile(t)
	for _, tc := range []struct{ name, column, want string }{
		{"nope", "", `relation "public"."nope" does not exist or is not a table or materialized view`},
		{"fresh_ids", "", `relation "public"."fresh_ids" does not exist or is not a table or materialized view`},
		{"secrets_label_idx", "", `relation "public"."secrets_label_idx" does not exist`},
		{"FRESH", "", `relation "public"."FRESH" does not exist`},
		{"fresh", "nope", `column "nope" of "public"."fresh" does not exist or the current role cannot SELECT it: ` +
			"pg_stats shows only the columns a role can SELECT"},
		{"secrets", "token", `column "token" of "public"."secrets" does not exist or the current role cannot SELECT it`},
		{"hidden", "", `the current role can SELECT no column of "public"."hidden": pg_stats shows only the columns a role can SELECT`},
	} {
		p, err := introspect.Profile(t.Context(), clean, "public", tc.name, tc.column)
		wantErr(t, err, tc.want)
		if p != nil {
			t.Errorf("Profile(%s, %q) = %+v, want nil", tc.name, tc.column, p)
		}
	}
}

// pg_stats shows nothing to a role that row-level security applies to: the
// sample reads only the rows the role may see.
func TestProfileSamplesRowsTheRoleMaySee(t *testing.T) {
	p := profile(t, testdb.Querier(t, testdb.RoleClean, testdb.DBApp), "app", "tenant_notes", "")
	if !p.Sampled || p.Sample != 25 || p.Rows != 50 {
		t.Fatalf("profile of tenant_notes as ro_clean = %+v", p)
	}
	if tenant := columns(p)["tenant"]; tenant.Distinct != 1 {
		t.Errorf("tenant = %+v, want only acme", tenant)
	}
	if owner := profile(t, testdb.Querier(t, testdb.Superuser, testdb.DBApp), "app", "tenant_notes", ""); owner.Sampled {
		t.Errorf("profile of tenant_notes as its owner = %+v, want pg_stats", owner)
	}
}

func TestProfileQueriesThroughTheQuerier(t *testing.T) {
	q := &fakeQuerier{res: oneValue(`{"Rows": 5, "Readable": 1, "Columns": [{"Name": "id", "Type": "integer", "Stats": {"NullFrac": 0, "Distinct": -1, "Width": 4}}]}`)}
	p, err := introspect.Profile(context.Background(), q, "app", "orders", "id")
	if err != nil {
		t.Fatal(err)
	}
	if q.got.SearchPath != "pg_catalog" || !reflect.DeepEqual(q.got.Params, []any{"app", "orders", "id", introspect.ProfileColumns}) {
		t.Errorf("query = %+v, want search_path pg_catalog and the names and column cap as parameters", q.got)
	}
	if p.Rows != 5 || len(p.Columns) != 1 || p.Columns[0].Stats == nil {
		t.Errorf("profile = %+v", p)
	}

	failure := errors.New("connection refused")
	if _, err := introspect.Profile(context.Background(), &fakeQuerier{err: failure}, "app", "orders", ""); !errors.Is(err, failure) {
		t.Errorf("error = %v, want it to wrap %v", err, failure)
	}
	truncated := &fakeQuerier{res: &result.Result{Truncated: true, TruncatedReason: "max_bytes=1048576"}}
	_, err = introspect.Profile(context.Background(), truncated, "app", "orders", "")
	wantErr(t, err, "catalog result exceeds the configured limits (max_bytes=1048576)")
	_, err = introspect.Profile(context.Background(), &fakeQuerier{res: &result.Result{}}, "app", "orders", "")
	wantErr(t, err, fmt.Sprintf("relation %q.%q does not exist or is not a table or materialized view", "app", "orders"))
}
