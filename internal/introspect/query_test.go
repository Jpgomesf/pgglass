package introspect_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/result"
)

// fakeQuerier returns a canned result and records the query it was given.
type fakeQuerier struct {
	res *result.Result
	err error
	got engine.Query
}

func (f *fakeQuerier) Query(_ context.Context, q engine.Query) (*result.Result, error) {
	f.got = q
	return f.res, f.err
}

// oneValue is a result with a single row holding v.
func oneValue(v string) *result.Result {
	return &result.Result{Columns: []result.Column{{Name: "json"}}, Rows: [][]*string{{&v}}}
}

// call runs each public query function against q and returns their errors.
func call(q engine.Querier) map[string]error {
	ctx := context.Background()
	_, _, schemasErr := introspect.FirstSchemas(ctx, q, 3)
	_, _, tablesErr := introspect.FirstTables(ctx, q, "app", 3)
	_, tableErr := introspect.Table(ctx, q, "app", "orders")
	_, searchErr := introspect.Search(ctx, q, "order", 5)
	return map[string]error{"FirstSchemas": schemasErr, "FirstTables": tablesErr, "Table": tableErr, "Search": searchErr}
}

func TestQueriesPinSearchPathAndPassParameters(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(engine.Querier) error
		want []any
	}{
		{"FirstSchemas", func(q engine.Querier) error { _, _, err := introspect.FirstSchemas(ctx, q, 3); return err }, []any{3}},
		{"FirstTables", func(q engine.Querier) error { _, _, err := introspect.FirstTables(ctx, q, "app", 3); return err }, []any{"app", 3}},
		{"Table", func(q engine.Querier) error { _, err := introspect.Table(ctx, q, "app", "orders"); return err }, []any{"app", "orders"}},
		// The pattern matches literally: ILIKE wildcards and the escape are escaped.
		{"Search", func(q engine.Querier) error { _, err := introspect.Search(ctx, q, `a_b%c\d`, 5); return err }, []any{`%a\_b\%c\\d%`, 5}},
	} {
		q := &fakeQuerier{res: oneValue("{}")}
		if tc.name == "Search" {
			q.res = oneValue("[]")
		}
		if err := tc.run(q); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if q.got.SearchPath != "pg_catalog" {
			t.Errorf("%s: search_path = %q, want pg_catalog", tc.name, q.got.SearchPath)
		}
		if !reflect.DeepEqual(q.got.Params, tc.want) {
			t.Errorf("%s: params = %#v, want %#v", tc.name, q.got.Params, tc.want)
		}
	}
}

func TestQuerierErrorsAreWrapped(t *testing.T) {
	failure := errors.New("connection refused")
	for name, err := range call(&fakeQuerier{err: failure}) {
		if !errors.Is(err, failure) {
			t.Errorf("%s: error = %v, want it to wrap %v", name, err, failure)
		}
	}
}

func TestTruncatedCatalogResultIsAnError(t *testing.T) {
	q := &fakeQuerier{res: &result.Result{Truncated: true, TruncatedReason: "max_bytes=1048576"}}
	for name, err := range call(q) {
		if err == nil || !strings.Contains(err.Error(), "catalog result exceeds the configured limits (max_bytes=1048576)") {
			t.Errorf("%s: error = %v, want the truncation reported", name, err)
		}
	}
}

func TestNoRowMeansNotFound(t *testing.T) {
	for _, res := range []*result.Result{{}, {Rows: [][]*string{{nil}}}} {
		errs := call(&fakeQuerier{res: res})
		wantErr(t, errs["FirstTables"], `schema "app" does not exist or the current role lacks USAGE on it`)
		wantErr(t, errs["Table"], `relation "app"."orders" does not exist or the current role cannot read it`)
		// Their aggregates always return a row, so none is a broken result.
		if errs["FirstSchemas"] == nil || errs["Search"] == nil {
			t.Errorf("FirstSchemas: %v, Search: %v, want errors", errs["FirstSchemas"], errs["Search"])
		}
	}
}

func TestUnexpectedJSONIsAnError(t *testing.T) {
	for _, value := range []string{
		`{"Schemas": [{"Name": "app", "Owner": "postgres"}], "Total": 1}`,
		`{"Schemas": [], "Count": 0}`,
		`{"Schemas": `,
		`[]`,
	} {
		_, _, err := introspect.FirstSchemas(context.Background(), &fakeQuerier{res: oneValue(value)}, 3)
		if err == nil {
			t.Errorf("FirstSchemas decoded %s without error", value)
		}
	}
}

func TestSearchRejectsEmptyPatternAndLimit(t *testing.T) {
	q := &fakeQuerier{err: errors.New("must not be queried")}
	if _, err := introspect.Search(context.Background(), q, "", 5); err == nil || err.Error() != "search pattern is empty" {
		t.Errorf("empty pattern: %v", err)
	}
	for _, limit := range []int{0, -1} {
		_, err := introspect.Search(context.Background(), q, "x", limit)
		wantErr(t, err, "search limit must be at least 1")
	}
}
