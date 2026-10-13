package introspect_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/result"
)

func TestTableReportsAMissingRelationAsErrNotFound(t *testing.T) {
	for _, res := range []*result.Result{{}, {Rows: [][]*string{{nil}}}} {
		_, err := introspect.Table(context.Background(), &fakeQuerier{res: res}, "app", "orders")
		if !errors.Is(err, introspect.ErrNotFound) ||
			err.Error() != `relation "app"."orders" does not exist or the current role cannot read it` {
			t.Errorf("Table of no row = %v, want introspect.ErrNotFound with its message", err)
		}
	}
	for _, q := range []*fakeQuerier{
		{err: errors.New("connection refused")},
		{res: &result.Result{Truncated: true, TruncatedReason: "max_bytes=1048576"}},
		{res: oneValue(`{"Owner": "postgres"}`)},
	} {
		for name, err := range call(q) {
			if errors.Is(err, introspect.ErrNotFound) {
				t.Errorf("%s: %v is introspect.ErrNotFound, want only a missing relation to be", name, err)
			}
		}
	}
}
