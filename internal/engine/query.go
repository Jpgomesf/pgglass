package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// typeNamesSQL names the types pgx does not know: enums, composites and
// extension types.
const typeNamesSQL = "SELECT oid, typname FROM pg_catalog.pg_type WHERE oid = ANY($1::pg_catalog.oid[])"

// run executes q alone in a READ ONLY transaction that it always rolls back.
func run(ctx context.Context, p *pgxpool.Pool, q Query, l Limits) (*result.Result, error) {
	tx, err := p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
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

// setup returns the statement that applies l, and searchPath when set, to
// the transaction. set_config(name, value, true) is SET LOCAL as a function.
// Calling it from a SELECT also takes the transaction's first snapshot, after
// which Postgres refuses to switch the transaction to READ WRITE.
//
// It also turns standard_conforming_strings on, whatever the role or database
// default: the guard parses SQL as Postgres does with it on, and with it off
// a backslash in a '...' literal could move where the literal ends.
func setup(l Limits, searchPath string) (string, []any) {
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

// collect runs q in tx and keeps its rows, as Postgres text, until one would
// exceed a limit. It then cancels the rest of the query instead of reading it.
func collect(ctx context.Context, tx pgx.Tx, q Query, l Limits) (*result.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// QueryExecModeExec already asks for text; asking explicitly keeps every
	// column in text under any mode.
	args := append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, q.Params...)
	rows, err := tx.Query(ctx, q.SQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c := collector{limits: l, res: &result.Result{Columns: columns(tx.Conn().TypeMap(), rows.FieldDescriptions())}}
	for rows.Next() {
		if !c.add(rows.RawValues()) {
			cancel() // rows.Close then waits only for the server to stop
			return c.res, nil
		}
	}
	if err := rows.Err(); err != nil {
		// A row wider than the per-message cap is a byte-budget overrun that
		// pgx caught before buffering it, so truncate rather than error, like
		// the collector's own MaxBytes cut.
		if oversizedRow(err) {
			c.truncate("max_bytes", c.limits.MaxBytes)
			return c.res, nil
		}
		return nil, err
	}
	return c.res, nil
}

// oversizedRow reports whether err is pgx rejecting a protocol message larger
// than the per-message cap harden set from max_bytes.
func oversizedRow(err error) bool {
	var e *pgproto3.ExceededMaxBodyLenErr
	return errors.As(err, &e)
}

// collector keeps rows until one would exceed a limit.
type collector struct {
	limits Limits
	res    *result.Result
	bytes  int
}

// add keeps one row of raw text values and reports whether it fit. A row that
// does not fit truncates the result.
func (c *collector) add(raw [][]byte) bool {
	size := 0
	for _, v := range raw {
		size += len(v)
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

// textRow copies one row of raw text values, which pgx reuses between rows.
// A nil value is NULL.
func textRow(raw [][]byte) []*string {
	row := make([]*string, len(raw))
	for i, v := range raw {
		if v != nil {
			s := string(v)
			row[i] = &s
		}
	}
	return row
}

// columns describes the result columns, naming the types pgx knows.
func columns(types *pgtype.Map, fields []pgconn.FieldDescription) []result.Column {
	cols := make([]result.Column, len(fields))
	for i, f := range fields {
		cols[i] = result.Column{Name: f.Name, TypeOID: f.DataTypeOID}
		if t, ok := types.TypeForOID(f.DataTypeOID); ok {
			cols[i].TypeName = t.Name
		}
	}
	return cols
}

// nameTypes looks up in pg_type the names of the column types pgx does not
// know. It runs after the query's transaction, which a truncation aborts.
func nameTypes(ctx context.Context, p *pgxpool.Pool, cols []result.Column) error {
	var unknown []uint32
	for _, c := range cols {
		if c.TypeName == "" {
			unknown = append(unknown, c.TypeOID)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	rows, err := p.Query(ctx, typeNamesSQL, unknown)
	if err != nil {
		return err
	}
	names := make(map[uint32]string, len(unknown))
	var oid uint32
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&oid, &name}, func() error {
		names[oid] = name
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

// rollback ends tx with a fresh context, so a cancelled query still rolls
// back. When ROLLBACK fails, pgx closes the connection, which ends the
// transaction on the server all the same.
func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// queryError reports err, the failure of a query run under ctx: as a context
// error once ctx has ended, even if Postgres answered the resulting cancel
// request; as a *QueryError when Postgres raised it; otherwise unchanged.
func queryError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("query cancelled: %w", ctx.Err())
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return &QueryError{
			SQLState: pgErr.Code,
			Message:  pgErr.Message,
			Detail:   pgErr.Detail,
			Hint:     pgErr.Hint,
			Position: int(pgErr.Position),
		}
	}
	return err
}
