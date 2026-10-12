package spill_test

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// t0 is the fake clock's time in tests that pin it.
var t0 = time.Date(2026, 10, 12, 10, 15, 30, 0, time.UTC)

func str(v string) *string { return &v }

// people is a small result with a NULL and an empty string.
func people() *result.Result {
	return &result.Result{
		Columns: []result.Column{
			{Name: "id", TypeOID: pgtype.Int4OID, TypeName: "int4"},
			{Name: "name", TypeOID: pgtype.TextOID, TypeName: "text"},
		},
		Rows: [][]*string{{str("1"), str("Ada")}, {str("2"), nil}, {str("3"), str("")}},
	}
}

// numbered returns n rows of (id, label), with label NULL in every tenth row.
func numbered(n int) *result.Result {
	res := &result.Result{Columns: []result.Column{
		{Name: "id", TypeOID: pgtype.Int4OID, TypeName: "int4"},
		{Name: "label", TypeOID: pgtype.TextOID, TypeName: "text"},
	}}
	for i := 1; i <= n; i++ {
		var label *string
		if i%10 != 0 {
			label = str(fmt.Sprintf("label, \"number\" %d", i))
		}
		res.Rows = append(res.Rows, []*string{str(strconv.Itoa(i)), label})
	}
	return res
}

func newStore(t *testing.T, inlineMaxBytes int) *spill.Store {
	t.Helper()
	return &spill.Store{
		Dir:            filepath.Join(t.TempDir(), "spill"),
		TTL:            24 * time.Hour,
		InlineMaxBytes: inlineMaxBytes,
		Now:            func() time.Time { return t0 },
	}
}

// inlineOf is what Output returns inline for res: the rendering with cells cut
// at 200 characters and the given footer.
func inlineOf(t *testing.T, res *result.Result, f render.Format, footer string) string {
	t.Helper()
	body, err := render.Render(res, f, 200)
	if err != nil {
		t.Fatal(err)
	}
	return body + footer
}

// spillPath returns the file path named in a summary.
func spillPath(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if path, ok := strings.CutPrefix(line, "Full result: "); ok {
			return path
		}
	}
	t.Fatalf("no file path in output:\n%s", out)
	return ""
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestOutputReturnsSmallResultsInline(t *testing.T) {
	store := newStore(t, 1000)
	out, err := store.Output("pg_query", "prod", people(), render.TSV)
	if err != nil {
		t.Fatal(err)
	}
	if want := "id\tname\n1\tAda\n2\t\\N\n3\t\n(3 rows)"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
	if _, err := os.Stat(store.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("inline output created the spill directory: %v", err)
	}
}

func TestOutputFooterCountsRowsAndNotesTruncation(t *testing.T) {
	one := numbered(1)
	truncated := numbered(2)
	truncated.Truncated, truncated.TruncatedReason = true, "max_rows=2"
	unexplained := numbered(2)
	unexplained.Truncated = true
	tests := []struct {
		name string
		res  *result.Result
		f    render.Format
		want string
	}{
		{"no rows", numbered(0), render.TSV, "id\tlabel\n(0 rows)"},
		{"one row", one, render.TSV, "id\tlabel\n1\tlabel, \"number\" 1\n(1 row)"},
		{"truncated", truncated, render.TSV, "(2 rows; truncated: max_rows=2)"},
		{"truncated without a reason", unexplained, render.TSV, "(2 rows; truncated)"},
		{"after json", one, render.JSON, `"rows":[[1,"label, \"number\" 1"]]}` + "\n(1 row)"},
		{"after markdown", one, render.Markdown, "| 1 | label, \"number\" 1 |\n(1 row)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := newStore(t, 1000).Output("pg_query", "prod", tt.res, tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(out, tt.want) {
				t.Errorf("got %q, want it to end with %q", out, tt.want)
			}
		})
	}
}

func TestOutputCutsInlineCellsAt200Characters(t *testing.T) {
	res := people()
	res.Rows[0][1] = str(strings.Repeat("é", 1000))
	out, err := newStore(t, 10_000).Output("pg_query", "prod", res, render.TSV)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\t" + strings.Repeat("é", 200) + "…[+800 chars]\n"
	if !strings.Contains(out, want) || strings.Contains(out, "Full result") {
		t.Errorf("want the cell cut inline, got %q", out)
	}
}

func TestOutputInlineBoundary(t *testing.T) {
	inline := inlineOf(t, people(), render.Markdown, "(3 rows)")

	atLimit := newStore(t, len(inline))
	out, err := atLimit.Output("pg_query", "prod", people(), render.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	if out != inline {
		t.Errorf("at the limit: got %q, want the inline rendering %q", out, inline)
	}

	overLimit := newStore(t, len(inline)-1)
	out, err = overLimit.Output("pg_query", "prod", people(), render.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spillPath(t, out)); err != nil {
		t.Errorf("one byte over the limit: want a spill file: %v", err)
	}
}

func TestOutputSpillsTheFullResultAsCSV(t *testing.T) {
	res := numbered(50)
	res.Rows[0][1] = str(strings.Repeat("wide ", 100))
	store := newStore(t, 200)

	out, err := store.Output("pg_query", "prod", res, render.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	path := spillPath(t, out)
	if filepath.Dir(path) != store.Dir {
		t.Errorf("spill file %s is not in %s", path, store.Dir)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "spill-pg_query-prod-20261012T101530Z-") || !strings.HasSuffix(base, ".csv") {
		t.Errorf("spill file name %q does not carry tool, connection and timestamp", base)
	}

	data, err := os.ReadFile(path) //nolint:gosec // a file the test's store wrote
	if err != nil {
		t.Fatal(err)
	}
	want, err := render.Render(res, render.CSV, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("spill file is not the full CSV rendering:\n%s", data)
	}
	records, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 51 || records[1][1] != strings.Repeat("wide ", 100) || records[50][0] != "50" {
		t.Errorf("CSV reads back as %d records, want a header and 50 full rows", len(records))
	}
}

func TestOutputSummary(t *testing.T) {
	res := &result.Result{
		Columns: []result.Column{
			{Name: "id", TypeOID: pgtype.Int8OID, TypeName: "int8"},
			{Name: "status", TypeOID: 16385, TypeName: "order_status"},
			{Name: "placed", TypeOID: pgtype.TimestamptzOID, TypeName: "timestamptz"},
			{Name: "note", TypeOID: pgtype.TextOID, TypeName: "text"},
		},
		Truncated:       true,
		TruncatedReason: "max_rows=7",
	}
	for i, row := range [][]string{
		{"1", "pending", "2026-01-02 00:00:00+00", "first\tline"},
		{"2", "paid", "2026-01-01 00:00:00+00", ""},
		{"3", "paid", "2026-03-01 12:00:00+00", strings.Repeat("x", 100)},
		{"4", "shipped", "", "NULL"},
		{"5", "paid", "2026-02-01 00:00:00+00", "note"},
		{"6", "pending", "2026-02-01 00:00:00+00", ""},
		{"7", "paid", "2026-02-15 08:30:00+00", "note"},
	} {
		cells := make([]*string, len(row))
		for j, v := range row {
			if v != "" || j != 2 {
				cells[j] = str(v)
			}
		}
		if i == 1 {
			cells[3] = nil
		}
		res.Rows = append(res.Rows, cells)
	}
	store := newStore(t, 100)

	out, err := store.Output("pg_query", "prod", res, render.TSV)
	if err != nil {
		t.Fatal(err)
	}
	path := spillPath(t, out)
	size := len(inlineOf(t, res, render.TSV, "(7 rows; truncated: max_rows=7)"))
	want := strings.Join([]string{
		fmt.Sprintf("Result too large to return inline (%d bytes, limit 100): 7 rows × 4 columns, truncated=true (max_rows=7).", size),
		"Full result: " + path,
		`It is CSV (RFC 4180) with a header row; NULL is an unquoted empty field and "" the empty string.`,
		"Use Read or Grep on the file instead of re-running the query.",
		"",
		"Columns:",
		"column\ttype\tnulls\tdistinct\tmin\tmax",
		"id\tint8\t0\t7\t1\t7",
		"status\torder_status\t0\t3\t\t",
		"placed\ttimestamptz\t1\t5\t2026-01-01 00:00:00+00\t2026-03-01 12:00:00+00",
		"note\ttext\t1\t5\t\t",
		"",
		"First 5 rows:",
		"id\tstatus\tplaced\tnote",
		"1\tpending\t2026-01-02 00:00:00+00\tfirst\\tline",
		"2\tpaid\t2026-01-01 00:00:00+00\t\\N",
		"3\tpaid\t2026-03-01 12:00:00+00\t" + strings.Repeat("x", 64) + "…[+36 chars]",
		"4\tshipped\t\\N\tNULL",
		"5\tpaid\t2026-02-01 00:00:00+00\tnote",
	}, "\n")
	if out != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out, want)
	}
}

func TestSpillFilePermissions(t *testing.T) {
	store := newStore(t, 10)
	store.Dir = filepath.Join(t.TempDir(), "a", "b")
	out, err := store.Output("pg_query", "prod", people(), render.TSV)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{store.Dir, filepath.Dir(store.Dir)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("directory %s has mode %o, want 700", dir, perm)
		}
	}
	info, err := os.Stat(spillPath(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spill file has mode %o, want 600", perm)
	}
}

func TestOutputMakesNamesSafe(t *testing.T) {
	tests := []struct {
		tool, conn string
		wantPrefix string
	}{
		{"pg_query", "../../etc/passwd", "spill-pg_query-______etc_passwd-"},
		{"pg/query", "prod*db", "spill-pg_query-prod_db-"},
		{"pg_query", "produção", "spill-pg_query-produ__o-"},
		{"pg_query", strings.Repeat("c", 100), "spill-pg_query-" + strings.Repeat("c", 32) + "-"},
		{"", "", "spill---"},
	}
	for _, tt := range tests {
		store := newStore(t, 10)
		out, err := store.Output(tt.tool, tt.conn, people(), render.TSV)
		if err != nil {
			t.Fatal(err)
		}
		path := spillPath(t, out)
		if filepath.Dir(path) != store.Dir || !strings.HasPrefix(filepath.Base(path), tt.wantPrefix+"20261012T101530Z-") {
			t.Errorf("tool %q, conn %q: spill file %s, want %s/%s...", tt.tool, tt.conn, path, store.Dir, tt.wantPrefix)
		}
	}
}

func TestConcurrentSpillsGetUniqueFiles(t *testing.T) {
	const n = 100
	store := newStore(t, 10)
	res := numbered(20)
	want, err := render.Render(res, render.CSV, 0)
	if err != nil {
		t.Fatal(err)
	}

	outs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = store.Output("pg_query", "prod", res, render.TSV)
		}()
	}
	wg.Wait()

	paths := map[string]bool{}
	for i, out := range outs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		path := spillPath(t, out)
		paths[path] = true
		data, err := os.ReadFile(path) //nolint:gosec // a file the test's store wrote
		if err != nil || string(data) != want {
			t.Errorf("%s: incomplete spill file (%v)", path, err)
		}
	}
	if len(paths) != n {
		t.Errorf("%d spills wrote %d distinct files", n, len(paths))
	}
	if names := dirNames(t, store.Dir); len(names) != n {
		t.Errorf("%d files in the spill directory, want %d", len(names), n)
	}
}

func TestOutputFallsBackToCappedInlineWhenTheFileCannotBeWritten(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := numbered(50)
	res.Truncated, res.TruncatedReason = true, "max_rows=50"
	firstRows := func(n int) *result.Result { return &result.Result{Columns: res.Columns, Rows: res.Rows[:n]} }

	for _, f := range []render.Format{render.TSV, render.CSV, render.Markdown, render.JSON} {
		t.Run(string(f), func(t *testing.T) {
			store := newStore(t, 600)
			store.Dir = filepath.Join(file, "spill") // below a regular file: cannot be created
			out, err := store.Output("pg_query", "prod", res, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > store.InlineMaxBytes {
				t.Errorf("output has %d bytes, more than InlineMaxBytes %d", len(out), store.InlineMaxBytes)
			}
			body, note, shown := splitCapped(t, out)
			if body != inlineOf(t, firstRows(shown), f, "") {
				t.Errorf("body is not the rendering of the first %d rows:\n%s", shown, body)
			}
			if f == render.JSON && !json.Valid([]byte(body)) {
				t.Errorf("capped JSON is not valid: %s", body)
			}
			// One row more must not fit next to the longest note, which is
			// what Output budgets for.
			longest := strings.Replace(note, fmt.Sprintf("(%d of", shown), "(50 of", 1)
			if len(inlineOf(t, firstRows(shown+1), f, ""))+len(longest) <= store.InlineMaxBytes {
				t.Errorf("%d rows shown, but %d fit", shown, shown+1)
			}
		})
	}
}

// splitCapped splits capped output of numbered(50) into the rendering, the
// note after it and the number of rows the note says it shows.
func splitCapped(t *testing.T, out string) (body, note string, shown int) {
	t.Helper()
	i := strings.LastIndex(out, "\n(")
	body, note = out[:i+1], out[i+1:]
	if !strings.Contains(note, "could not write the spill file") || !strings.HasSuffix(note, "; truncated: max_rows=50)") {
		t.Fatalf("want a note naming the failure, got %q", out)
	}
	if _, err := fmt.Sscanf(note, "(%d of 50 rows shown;", &shown); err != nil || shown < 1 || shown >= 50 {
		t.Fatalf("want some but not all rows shown, got %q (%v)", note, err)
	}
	return body, note, shown
}

func TestOutputFallbackStaysBoundedWhenNothingFits(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "file-not-dir")} {
		if dir != "" {
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		store := newStore(t, 10)
		store.Dir = dir
		out, err := store.Output("pg_query", "prod", numbered(1000), render.TSV)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "(0 of 1000 rows shown; could not write the spill file: ") || len(out) > 300 {
			t.Errorf("Dir %q: want only a short note, got %q", dir, out)
		}
	}
}

func TestOutputRejectsAnUnknownFormat(t *testing.T) {
	store := newStore(t, 10)
	if _, err := store.Output("pg_query", "prod", people(), render.Format("xml")); err == nil {
		t.Error("want an error for an unknown format")
	}
	if _, err := store.Output("pg_query", "prod", nil, render.TSV); err == nil {
		t.Error("want an error for a nil result")
	}
	if _, err := os.Stat(store.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed Output created the spill directory: %v", err)
	}
}

// touch creates name under dir with the given modification time.
func touch(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCleanupRemovesOnlyItsOwnStaleFiles(t *testing.T) {
	// The clock runs two days ahead, so the directory and the symlink below,
	// whose times the test cannot set, are stale too.
	now := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	store := newStore(t, 10)
	store.Now = func() time.Time { return now }
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-25 * time.Hour)
	touch(t, store.Dir, "spill-pg_query-prod-20261011T091530Z-123.csv", stale)
	touch(t, store.Dir, "spill-pg_schema-x-20261011T091530Z-9.csv", now.Add(-24*time.Hour-time.Second))
	kept := []string{
		touch(t, store.Dir, "spill-pg_query-prod-20261012T091530Z-456.csv", now.Add(-time.Hour)),
		touch(t, store.Dir, "spill-pg_query-prod-20261011T101530Z-789.csv", now.Add(-24*time.Hour)), // exactly TTL old
		touch(t, store.Dir, "notes.csv", stale),
		touch(t, store.Dir, "spill-notes.txt", stale),
		touch(t, store.Dir, "spill-notes.csv", stale),
		touch(t, store.Dir, "my-spill-pg_query-prod-20261011T091530Z-1.csv", stale),
	}
	ownDir := filepath.Join(store.Dir, "spill-x-20261011T091530Z-2.csv")
	if err := os.Mkdir(ownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := touch(t, t.TempDir(), "target.csv", stale)
	link := filepath.Join(store.Dir, "spill-x-20261011T091530Z-3.csv")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, ownDir, outside, link)

	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, path := range kept {
		if filepath.Dir(path) == store.Dir {
			want = append(want, filepath.Base(path))
		}
	}
	slices.Sort(want)
	if got := dirNames(t, store.Dir); !slices.Equal(got, want) {
		t.Errorf("after Cleanup: %q, want %q", got, want)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Errorf("Cleanup followed a symlink: %v", err)
	}
}

func TestOutputNamesTheFileByAbsolutePath(t *testing.T) {
	t.Chdir(t.TempDir())
	store := newStore(t, 10)
	store.Dir = "spill"
	out, err := store.Output("pg_query", "prod", people(), render.TSV)
	if err != nil {
		t.Fatal(err)
	}
	path := spillPath(t, out)
	if !filepath.IsAbs(path) {
		t.Errorf("relative Dir gave a relative path %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
}

func TestCleanupReportsAnUnreadableDir(t *testing.T) {
	store := newStore(t, 10)
	store.Dir = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(store.Dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err == nil {
		t.Error("want an error when Dir is not a directory")
	}
}

func TestCleanupWithoutWorkToDo(t *testing.T) {
	missing := newStore(t, 10)
	if err := missing.Cleanup(); err != nil {
		t.Errorf("missing directory: %v", err)
	}
	if err := (&spill.Store{}).Cleanup(); err != nil {
		t.Errorf("zero Store: %v", err)
	}

	keep := newStore(t, 10)
	keep.TTL = 0
	if err := os.MkdirAll(keep.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	touch(t, keep.Dir, "spill-pg_query-prod-20201011T091530Z-1.csv", t0.AddDate(-5, 0, 0))
	if err := keep.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, keep.Dir); len(got) != 1 {
		t.Errorf("TTL 0 must keep spill files, left %q", got)
	}
}

func TestOutputCleansUpAtMostOnceAnHour(t *testing.T) {
	now := t0
	store := newStore(t, 1000)
	store.Now = func() time.Time { return now }
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	output := func() {
		t.Helper()
		if _, err := store.Output("pg_query", "prod", people(), render.TSV); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	first := touch(t, store.Dir, "spill-pg_query-prod-20261010T000000Z-1.csv", now.Add(-48*time.Hour))
	output()
	if exists(first) {
		t.Fatal("the first Output must clean up stale files")
	}

	second := touch(t, store.Dir, "spill-pg_query-prod-20261010T000000Z-2.csv", now.Add(-48*time.Hour))
	now = now.Add(59 * time.Minute)
	output()
	if !exists(second) {
		t.Fatal("Output cleaned up again within the hour")
	}

	now = now.Add(time.Minute)
	output()
	if exists(second) {
		t.Fatal("Output did not clean up an hour after the last cleanup")
	}
}
