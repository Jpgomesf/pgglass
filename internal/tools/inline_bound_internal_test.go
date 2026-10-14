package tools

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/spill"
)

func quotingError(n int) error {
	return &dbError{&engine.QueryError{SQLState: "22P02", Hint: "Cast it to text first.",
		Message: `invalid input syntax for type integer: "` + strings.Repeat("y", n) + `"`}}
}

func tenReports() string {
	var b strings.Builder
	for i := range 10 {
		b.WriteString("c" + strconv.Itoa(i) + ": role check: role ro: 1 finding(s)\n  warning table_write: " + strings.Repeat("x", 100) + "\n\n")
	}
	return b.String()
}

// A notice past its limit keeps the lines that fit, then points to
// pg_list_connections.
func TestFitNotice(t *testing.T) {
	notice := tenReports()
	if got := fitNotice(notice, len(notice)); got != notice {
		t.Errorf("a notice that fits was cut:\n%s", got)
	}
	got := fitNotice(notice, 500)
	kept, ok := strings.CutSuffix(got, "… more role check findings: pg_list_connections shows them all\n\n")
	if len(got) > 500 || !ok || kept == "" || !strings.HasPrefix(notice, kept) || !strings.HasSuffix(kept, "\n") {
		t.Errorf("notice cut to 500: %d bytes:\n%s", len(got), got)
	}
}

// room cuts the notice to half the inline limit and leaves the rest.
func TestRoom(t *testing.T) {
	h := &Handlers{spill: &spill.Store{InlineMaxBytes: 1000}}
	short := "role check: role ro: 1 finding(s)\n  warning createdb: can create databases\n\n"
	c := &call{notice: short}
	if room := h.room(c); room != 1000-len(short) || c.notice != short {
		t.Errorf("room = %d, notice %q; want %d and the notice as it was", room, c.notice, 1000-len(short))
	}
	c = &call{notice: tenReports()}
	if room := h.room(c); len(c.notice) > 500 || room != 1000-len(c.notice) ||
		!strings.HasSuffix(c.notice, "pg_list_connections shows them all\n\n") {
		t.Errorf("room = %d after a notice of %d bytes:\n%s", room, len(c.notice), c.notice)
	}
}

// Failed connections' lines share half the room, each cut in the middle to its
// share, and the rows take the rest.
func TestFanoutTextFitsFailureLines(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: 1000}}
	var values []string
	for i := range 90 {
		values = append(values, strconv.Itoa(i))
	}
	got, err := h.fanoutText([]fanoutResult{fanoutOK("a", fanoutIDColumn, values...), {conn: "b", err: quotingError(5000)}}, render.TSV, 1000)
	rows, line, _ := strings.Cut(got, "\n\n")
	if err != nil || len(got) > 1000 || !strings.HasPrefix(rows, "connection\tid\na\t0\n") || !strings.HasSuffix(rows, "\n(90 rows)") ||
		!strings.HasPrefix(line, `b: ERROR: invalid input syntax for type integer: "yyy`) ||
		!strings.HasSuffix(line, `yyy" (SQLSTATE 22P02)`+"\n  HINT: Cast it to text first.") || len(line) >= 500 {
		t.Errorf("rows and a failure = %v, %d bytes:\n%s", err, len(got), got)
	}

	_, err = h.fanoutText([]fanoutResult{{conn: "a", err: quotingError(5000)}, {conn: "b", err: quotingError(5000)}}, render.TSV, 1000)
	text := errorText(err)
	if err == nil || len(text) > 1000 || !strings.HasPrefix(text, "a: ERROR: ") || !strings.Contains(text, "\nb: ERROR: ") ||
		strings.Count(text, `" (SQLSTATE 22P02)`) != 2 || strings.Count(text, " bytes cut]…") != 2 {
		t.Errorf("every connection failed = %d bytes:\n%s", len(text), text)
	}
}
