package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// The API other packages build against. Changing it must fail here first.
var (
	_ func(*config.Config) *engine.Manager                                                 = engine.New
	_ func(*engine.Manager, context.Context, string, engine.Query) (*result.Result, error) = (*engine.Manager).Query
	_ func(*engine.Manager, string) engine.Querier                                         = (*engine.Manager).Querier
	_ func(*engine.Manager)                                                                = (*engine.Manager).Close
	_ error                                                                                = (*engine.QueryError)(nil)

	_ = engine.Limits{StatementTimeout: time.Second, LockTimeout: time.Second, MaxRows: 1, MaxBytes: 1}
	_ = engine.Query{SQL: "SELECT 1", Params: []any{1}, SearchPath: "app", MaxRows: 10}
	_ = engine.QueryError{SQLState: "42P01", Message: "m", Detail: "d", Hint: "h", Position: 1}
)

func TestQueryErrorMessage(t *testing.T) {
	err := &engine.QueryError{SQLState: "42P01", Message: `relation "nope" does not exist`, Position: 15}
	if got, want := err.Error(), `relation "nope" does not exist (SQLSTATE 42P01)`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestBoundQuerierDelegatesToManager(t *testing.T) {
	m := engine.New(&config.Config{})
	defer m.Close()
	q := engine.Query{SQL: "SELECT 1"}

	_, direct := m.Query(context.Background(), "missing", q)
	_, bound := m.Querier("missing").Query(context.Background(), q)
	if direct == nil || bound == nil || direct.Error() != bound.Error() {
		t.Errorf("Manager.Query error %v, bound Querier error %v: want the same error", direct, bound)
	}
}
