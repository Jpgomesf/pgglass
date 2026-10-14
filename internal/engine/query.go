package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/result"
)

const typeNamesSQL = "SELECT oid, typname FROM pg_catalog.pg_type WHERE oid = ANY($1::pg_catalog.oid[])"

func run(ctx context.Context, p connPool, q Query, l limits) (*result.Result, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()
	tx, err := c.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)

	sql, args := setup(l, q.SearchPath)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return nil, err
	}
	return collect(ctx, tx, q, l)
}

func setup(l limits, searchPath string) (string, []any) {
	sql := "SELECT pg_catalog.set_config('statement_timeout', $1, true)," +
		" pg_catalog.set_config('lock_timeout', $2, true)," +
		" pg_catalog.set_config('idle_in_transaction_session_timeout', $3, true)," +
		" pg_catalog.set_config('standard_conforming_strings', 'on', true)"
	args := []any{millis(l.StatementTimeout), millis(l.LockTimeout), millis(l.StatementTimeout)}
	if searchPath != "" {
		sql += ", pg_catalog.set_config('search_path', $4, true)"
		args = append(args, pgx.Identifier{searchPath}.Sanitize())
	}
	return sql, args
}

func millis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func collect(ctx context.Context, tx pgx.Tx, q Query, l limits) (*result.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, q.Params...)
	rows, err := tx.Query(ctx, q.SQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c := collector{limits: l, res: &result.Result{Columns: columns(tx.Conn().TypeMap(), rows.FieldDescriptions())}}
	for rows.Next() {
		if !c.add(rows.RawValues()) {
			cancel()
			return c.res, nil
		}
	}
	if err := rows.Err(); err != nil {
		var tooBig *pgproto3.ExceededMaxBodyLenErr
		if !errors.As(err, &tooBig) {
			return nil, err
		}
		if len(c.res.Columns) == 0 {
			return nil, fmt.Errorf("the server sent a %d-byte message, over the %d-byte limit (max_bytes plus 1 MiB); the connection was reset",
				tooBig.ActualBodyLen, tooBig.MaxExpectedBodyLen)
		}
		c.truncate("max_bytes", c.limits.MaxBytes)
		return c.res, nil
	}
	return c.res, nil
}

type collector struct {
	limits limits
	res    *result.Result
	bytes  int
}

const valueCost = 24

func (c *collector) add(raw [][]byte) bool {
	size := 0
	for _, v := range raw {
		size += len(v) + valueCost
	}
	switch {
	case len(c.res.Rows) >= c.limits.MaxRows:
		c.truncate("max_rows", c.limits.MaxRows)
		return false
	case c.bytes+size > c.limits.MaxBytes:
		c.truncate("max_bytes", c.limits.MaxBytes)
		return false
	}
	c.bytes += size
	c.res.Rows = append(c.res.Rows, textRow(raw))
	return true
}

func (c *collector) truncate(limit string, value int) {
	c.res.Truncated = true
	c.res.TruncatedReason = fmt.Sprintf("%s=%d", limit, value)
}

func textRow(raw [][]byte) []*string {
	row := make([]*string, len(raw))
	for i, v := range raw {
		if v != nil {
			s := validUTF8(string(v))
			row[i] = &s
		}
	}
	return row
}

func validUTF8(s string) string {
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

func columns(types *pgtype.Map, fields []pgconn.FieldDescription) []result.Column {
	cols := make([]result.Column, len(fields))
	for i, f := range fields {
		cols[i] = result.Column{Name: validUTF8(f.Name), TypeOID: f.DataTypeOID}
		if t, ok := types.TypeForOID(f.DataTypeOID); ok {
			cols[i].TypeName = t.Name
		}
	}
	return cols
}

func nameTypes(ctx context.Context, p connPool, cols []result.Column) error {
	var unknown []uint32
	for _, c := range cols {
		if c.TypeName == "" {
			unknown = append(unknown, c.TypeOID)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	conn, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, typeNamesSQL, unknown)
	if err != nil {
		return err
	}
	names := make(map[uint32]string, len(unknown))
	var oid uint32
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&oid, &name}, func() error {
		names[oid] = validUTF8(name)
		return nil
	}); err != nil {
		return err
	}
	for i := range cols {
		if cols[i].TypeName == "" {
			cols[i].TypeName = names[cols[i].TypeOID]
		}
	}
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func queryError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("query cancelled: %w", ctx.Err())
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return &QueryError{
			SQLState: pgErr.Code,
			Message:  validUTF8(pgErr.Message),
			Detail:   validUTF8(pgErr.Detail),
			Hint:     validUTF8(pgErr.Hint),
			Position: int(pgErr.Position),
		}
	}
	return err
}
