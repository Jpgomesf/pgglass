package engine_test

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

// A single wide field must not be buffered past the byte budget. pgx rejects
// a protocol message larger than the per-message cap after reading its 5-byte
// header, so the field is never allocated and the result is truncated.
//
// Not parallel: it reads process-wide allocation counters, which a concurrent
// test would inflate.
func TestSingleWideFieldTruncatesWithoutBuffering(t *testing.T) {
	const (
		maxBytes  = 1 << 20  // 1 MiB budget
		fieldSize = 64 << 20 // 64 MiB single value, 64x the cap
	)
	m := cleanApp(t, config.Defaults{MaxBytes: maxBytes})
	sql := "SELECT repeat('x', " + strconv.Itoa(fieldSize) + ")"

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res := mustQuery(t, m, engine.Query{SQL: sql})
	runtime.ReadMemStats(&after)

	if !res.Truncated || res.TruncatedReason != "max_bytes="+strconv.Itoa(maxBytes) {
		t.Fatalf("result not truncated by max_bytes: truncated=%v reason=%q", res.Truncated, res.TruncatedReason)
	}
	if len(res.Rows) != 0 {
		t.Errorf("kept %d rows, want 0: the oversized row must not be buffered", len(res.Rows))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > fieldSize/2 {
		t.Errorf("allocated %d bytes for one query; the %d-byte field was buffered despite the cap", allocated, fieldSize)
	}

	// The pool recovers after the broken connection: the next query works.
	if got := value(t, m, "SELECT 1"); got != "1" {
		t.Errorf("after the oversized row, SELECT 1 = %q, want 1", got)
	}
}

// A row whose values sum within the budget is kept whole: the per-message cap
// (max_bytes plus headroom) does not false-trip on a legitimate wide row.
func TestWideRowWithinBudgetIsKept(t *testing.T) {
	t.Parallel()
	const maxBytes = 8 << 20 // 8 MiB budget
	m := cleanApp(t, config.Defaults{MaxBytes: maxBytes})

	res := mustQuery(t, m, engine.Query{SQL: "SELECT repeat('x', 4 * 1024 * 1024)"}) // 4 MiB, under the budget
	if res.Truncated {
		t.Errorf("a 4 MiB field under an 8 MiB budget was truncated: %q", res.TruncatedReason)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] == nil || len(*res.Rows[0][0]) != 4*1024*1024 {
		t.Errorf("want one 4 MiB value, got %d rows", len(res.Rows))
	}
}
