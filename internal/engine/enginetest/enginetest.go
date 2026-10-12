// Package enginetest provides a minimal engine.Querier for testing packages
// that consume one (preflight, introspect) independently of engine.Manager.
package enginetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// Querier returns an engine.Querier that runs each query on dsn in its own
// READ ONLY transaction, always rolled back. Values come back in Postgres
// text format; TypeName is set only for types pgx knows (built-ins). It
// applies no limits and no guard, and returns Postgres errors as
// *pgconn.PgError, not *engine.QueryError. Its pool closes when t finishes.
func Querier(t testing.TB, dsn string) engine.Querier {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("enginetest: %v", err)
	}
	t.Cleanup(pool.Close)
	return querier{pool: pool}
}

type querier struct {
	pool *pgxpool.Pool
}

func (q querier) Query(ctx context.Context, query engine.Query) (*result.Result, error) {
	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if query.SearchPath != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", pgx.Identifier{query.SearchPath}.Sanitize()); err != nil {
			return nil, err
		}
	}
	// The simple protocol returns every value in text format.
	args := append([]any{pgx.QueryExecModeSimpleProtocol}, query.Params...)
	rows, err := tx.Query(ctx, query.SQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &result.Result{Columns: columns(tx.Conn().TypeMap(), rows.FieldDescriptions())}
	for rows.Next() {
		res.Rows = append(res.Rows, textValues(rows.RawValues()))
	}
	return res, rows.Err()
}

func columns(types *pgtype.Map, fields []pgconn.FieldDescription) []result.Column {
	cols := make([]result.Column, len(fields))
	for i, f := range fields {
		cols[i] = result.Column{Name: f.Name, TypeOID: f.DataTypeOID}
		if typ, ok := types.TypeForOID(f.DataTypeOID); ok {
			cols[i].TypeName = typ.Name
		}
	}
	return cols
}

// textValues copies one row of raw text values, which pgx reuses between rows.
func textValues(raw [][]byte) []*string {
	row := make([]*string, len(raw))
	for i, v := range raw {
		if v != nil {
			s := string(v)
			row[i] = &s
		}
	}
	return row
}
