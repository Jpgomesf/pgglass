package engine_test

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const headroom = 1 << 20

// A field just past the per-message cap is never buffered: the result is
// truncated, the connection replaced. Not parallel: it counts allocations.
func TestFieldPastTheMessageCapIsNotBuffered(t *testing.T) {
	const (
		maxBytes  = 1 << 20
		fieldSize = maxBytes + headroom + 1
	)
	m := cleanApp(t, config.Defaults{MaxBytes: maxBytes})
	pid := value(t, m, "SELECT pg_backend_pid()")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res := mustQuery(t, m, engine.Query{SQL: "SELECT repeat('x', " + strconv.Itoa(fieldSize) + ")"})
	runtime.ReadMemStats(&after)

	if !res.Truncated || res.TruncatedReason != "max_bytes="+strconv.Itoa(maxBytes) || len(res.Rows) != 0 {
		t.Fatalf("%d rows, truncated %v %q; want 0 rows, max_bytes=%d", len(res.Rows), res.Truncated, res.TruncatedReason, maxBytes)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > fieldSize/2 {
		t.Errorf("allocated %d bytes for one query; the %d-byte field was buffered despite the cap", allocated, fieldSize)
	}
	if got := value(t, m, "SELECT pg_backend_pid()"); got == pid {
		t.Errorf("backend %s serves the next query: the connection was not reset", got)
	}
}

// The rows before an oversized one are kept, and the server stops the query
// once the connection is gone.
func TestRowsBeforeAnOversizedRowAreKept(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxBytes: 1 << 20})
	pid := value(t, m, "SELECT pg_backend_pid()")

	res := mustQuery(t, m, engine.Query{SQL: "SELECT CASE WHEN g = 3 THEN repeat('x', 64000000) ELSE 'a' END FROM generate_series(1, 5) g"})
	if len(res.Rows) != 2 || deref(res.Rows[1][0]) != "a" || res.TruncatedReason != "max_bytes=1048576" {
		t.Errorf("%d rows, truncated %q; want the 2 rows before the oversized one, max_bytes=1048576", len(res.Rows), res.TruncatedReason)
	}
	watch := testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	eventually(t, "the server to end the query", func() bool { return running(t, watch, "repeat('x', 64000000)") == 0 })
	if got := value(t, m, "SELECT pg_backend_pid()"); got == pid {
		t.Errorf("backend %s serves the next query: the connection was not reset", got)
	}
}

// A row that fills the byte budget exactly is kept whole: the per-message cap
// never refuses a row the budget allows. Each value costs its length plus 24.
func TestRowFillingTheBudgetIsKept(t *testing.T) {
	t.Parallel()
	const maxBytes = 8 << 20
	m := cleanApp(t, config.Defaults{MaxBytes: maxBytes})
	row := func(n int) string { return "SELECT repeat('x', " + strconv.Itoa(n) + "), NULL::text, ''::text" }

	res := mustQuery(t, m, engine.Query{SQL: row(maxBytes - 3*24)})
	if res.Truncated || len(res.Rows) != 1 || len(deref(res.Rows[0][0])) != maxBytes-3*24 || res.Rows[0][1] != nil || deref(res.Rows[0][2]) != "" {
		t.Errorf("truncated %q, %d rows; want the one row whole", res.TruncatedReason, len(res.Rows))
	}
	res = mustQuery(t, m, engine.Query{SQL: row(maxBytes - 3*24 + 1)})
	if !res.Truncated || len(res.Rows) != 0 {
		t.Errorf("one byte over the budget: truncated %v, %d rows; want 0 rows", res.Truncated, len(res.Rows))
	}
}

// An oversized message before any row is not a row but an error or a notice,
// here an error quoting a 3 MB invalid value: an error, not an empty result.
func TestOversizedErrorIsAnError(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxBytes: 1 << 20})
	for _, q := range []engine.Query{
		{SQL: "SELECT repeat('x', 3000000)::int"},
		{SQL: "SELECT $1::int", Params: []any{strings.Repeat("x", 3_000_000)}},
	} {
		res, err := m.Query(t.Context(), db, q)
		if err == nil || !strings.Contains(err.Error(), "the connection was reset") {
			t.Errorf("%s: result %+v, err %.200v; want an error naming the oversized message", q.SQL, res, err)
		}
	}
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT repeat('x', 30)::int"})
	wantSQLState(t, err, "22P02")
}

// With a small budget, the messages that are not rows still fit in the cap:
// a row description longer than max_bytes, and an error.
func TestSmallBudgetStillReadsDescriptionsAndErrors(t *testing.T) {
	t.Parallel()
	m := cleanApp(t, config.Defaults{MaxBytes: 100})
	name := strings.Repeat("c", 62)
	res := mustQuery(t, m, engine.Query{SQL: fmt.Sprintf("SELECT 'a' AS %[1]s1, 'b' AS %[1]s2, 'c' AS %[1]s3", name)})
	if len(res.Columns) != 3 || len(res.Rows) != 1 || res.Truncated {
		t.Errorf("%d columns, %d rows, truncated %q; want 3 columns and 1 row", len(res.Columns), len(res.Rows), res.TruncatedReason)
	}
	_, err := m.Query(t.Context(), db, engine.Query{SQL: "SELECT 'x'::jsonb"})
	wantSQLState(t, err, "22P02")
}
