package tools

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/guard"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/spill"
)

var (
	fanoutIDColumn   = result.Column{Name: "id", TypeOID: pgtype.Int4OID, TypeName: "int4"}
	fanoutNameColumn = result.Column{Name: "name", TypeOID: pgtype.TextOID, TypeName: "text"}
)

// fanoutOK is a fanoutResult whose rows hold one value each.
func fanoutOK(conn string, col result.Column, values ...string) fanoutResult {
	res := &result.Result{Columns: []result.Column{col}}
	for _, v := range values {
		res.Rows = append(res.Rows, []*string{ptr(v)})
	}
	return fanoutResult{conn: conn, res: res}
}

func TestCheckFanout(t *testing.T) {
	conns := map[string]config.Connection{}
	var eleven []string
	for i := range 11 {
		name := fmt.Sprintf("c%02d", i)
		conns[name] = config.Connection{}
		eleven = append(eleven, name)
	}
	h := &Handlers{cfg: config.Config{Connections: conns}}
	for _, names := range [][]string{{"c00"}, {"c03", "c01"}, eleven[:10]} {
		if err := h.checkFanout(queryArgs{Connections: names}); err != nil {
			t.Errorf("checkFanout(%v) = %v", names, err)
		}
	}
	for _, tc := range []struct {
		args queryArgs
		want string
	}{
		{queryArgs{Connection: "c00", Connections: []string{"c01"}}, "pass connection or connections, not both"},
		{queryArgs{Connections: []string{}}, "connections must list 1 to 10 names, got 0"},
		{queryArgs{Connections: eleven}, "connections must list 1 to 10 names, got 11"},
		{queryArgs{Connections: []string{"c00", "nope"}}, `unknown connection "nope" (available: c00, c01, `},
		{queryArgs{Connections: []string{"c00", "c01", "c00"}}, `connections lists "c00" twice`},
	} {
		if err := h.checkFanout(tc.args); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("checkFanout(%+v) = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestSameFanoutColumns(t *testing.T) {
	status := result.Column{Name: "status", TypeOID: 16385, TypeName: "order_status"}
	statusElsewhere := status
	statusElsewhere.TypeOID = 24601 // the same enum, created in another database
	for _, tc := range []struct {
		name string
		cols [][]result.Column
		want bool
	}{
		{"one result", [][]result.Column{{fanoutIDColumn}}, true},
		{"same", [][]result.Column{{fanoutIDColumn, status}, {fanoutIDColumn, status}, {fanoutIDColumn, status}}, true},
		{"no columns", [][]result.Column{{}, {}}, true},
		{"enum OIDs differ", [][]result.Column{{fanoutIDColumn, status}, {fanoutIDColumn, statusElsewhere}}, true},
		{"name", [][]result.Column{{fanoutIDColumn}, {{Name: "n", TypeOID: pgtype.Int4OID, TypeName: "int4"}}}, false},
		{"type", [][]result.Column{{fanoutIDColumn}, {{Name: "id", TypeOID: pgtype.Int8OID, TypeName: "int8"}}}, false},
		{"count", [][]result.Column{{fanoutIDColumn}, {fanoutIDColumn, status}}, false},
		{"order", [][]result.Column{{fanoutIDColumn, status}, {status, fanoutIDColumn}}, false},
		{"the third", [][]result.Column{{fanoutIDColumn}, {fanoutIDColumn}, {fanoutNameColumn}}, false},
	} {
		var ok []fanoutResult
		for _, cols := range tc.cols {
			ok = append(ok, fanoutResult{res: &result.Result{Columns: cols}})
		}
		if got := sameFanoutColumns(ok); got != tc.want {
			t.Errorf("%s: sameFanoutColumns = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestMergeFanout(t *testing.T) {
	a, b, c := fanoutOK("a", fanoutIDColumn, "1", "2"), fanoutOK("b", fanoutIDColumn), fanoutOK("c", fanoutIDColumn, "3")
	a.res.Truncated, a.res.TruncatedReason = true, "max_rows=2"
	c.res.Rows = append(c.res.Rows, []*string{nil})
	c.res.Truncated, c.res.TruncatedReason = true, "max_bytes=100"

	merged := mergeFanout([]fanoutResult{a, b, c})
	got, err := render.Render(merged, render.JSON, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"columns":[{"name":"connection","type":"text"},{"name":"id","type":"int4"}],` +
		`"rows":[["a",1],["a",2],["c",3],["c",null]]}` + "\n"; got != want {
		t.Errorf("merged:\n%s\nwant\n%s", got, want)
	}
	if !merged.Truncated || merged.TruncatedReason != "a max_rows=2, c max_bytes=100" {
		t.Errorf("merged truncation = %t %q", merged.Truncated, merged.TruncatedReason)
	}
	if len(a.res.Columns) != 1 || len(a.res.Rows[0]) != 1 {
		t.Errorf("mergeFanout changed a result it read: %+v", a.res)
	}

	if whole := mergeFanout([]fanoutResult{b}); whole.Truncated || whole.TruncatedReason != "" || len(whole.Rows) != 0 {
		t.Errorf("mergeFanout of an untruncated result = %+v", whole)
	}
}

func TestFanoutNotices(t *testing.T) {
	got := fanoutNotices([]fanoutResult{
		{conn: "a", notice: "role check: role ro_a: 1 finding(s)\n  warning x: y\n\n"},
		{conn: "b"},
		{conn: "c", notice: "role check failed (role_check warn, so the call goes on): boom\n\n"},
	})
	want := "a: role check: role ro_a: 1 finding(s)\n  warning x: y\n\n" +
		"c: role check failed (role_check warn, so the call goes on): boom\n\n"
	if got != want {
		t.Errorf("fanoutNotices =\n%q\nwant\n%q", got, want)
	}
}

func TestFanoutText(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: 1000}}
	rejected := &guard.Violation{Reason: "schema net is not allowed as the search_path: its functions are denied"}
	failedLines := `b: ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8` + "\n" +
		`  HINT: Perhaps you meant to reference the column "customers.name".` + "\n" +
		"d: query rejected: schema net is not allowed as the search_path: its functions are denied"

	got, err := h.fanoutText([]fanoutResult{
		fanoutOK("a", fanoutIDColumn, "1"),
		{conn: "b", err: &dbError{queryErr}},
		fanoutOK("c", fanoutIDColumn, "2", "3"),
		{conn: "d", err: rejected},
	}, render.TSV, 1000)
	if want := "connection\tid\na\t1\nc\t2\nc\t3\n(3 rows)\n\n" + failedLines; err != nil || got != want {
		t.Errorf("merged with failures = %v:\n%s\nwant\n%s", err, got, want)
	}

	got, err = h.fanoutText([]fanoutResult{fanoutOK("a", fanoutIDColumn, "1"), fanoutOK("b", fanoutNameColumn, "x")}, render.TSV, 1000)
	want := "Columns differ across connections, so each has its own result.\n\n" +
		"a:\nid\n1\n(1 row)\n\n" +
		"b:\nname\nx\n(1 row)"
	if err != nil || got != want {
		t.Errorf("columns differ = %v:\n%s\nwant\n%s", err, got, want)
	}

	_, err = h.fanoutText([]fanoutResult{{conn: "b", err: &dbError{queryErr}}, {conn: "d", err: rejected}}, render.TSV, 1000)
	if err == nil || errorText(err) != failedLines {
		t.Errorf("all failed = %v, want an error showing\n%s", err, failedLines)
	}
}

// Sections share the inline limit, less the line that introduces them and
// their names: one that would fit alone spills when it does not fit its
// share.
func TestFanoutSectionsShareTheInlineLimit(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: 1000}}
	var values []string
	for i := range 60 {
		values = append(values, strconv.Itoa(i))
	}
	ids, names := fanoutOK("ids", fanoutIDColumn, values...), fanoutOK("names", fanoutNameColumn, values...)
	got, err := h.fanoutText([]fanoutResult{ids, names}, render.TSV, 1000)
	if err != nil || strings.Contains(got, "Full result") {
		t.Fatalf("two sections of about 180 bytes, sharing 1000 = %v:\n%s", err, got)
	}

	ok := []fanoutResult{ids, names}
	for i := range 8 {
		ok = append(ok, fanoutOK(fmt.Sprintf("more%d", i), fanoutNameColumn, "x"))
	}
	if got, err = h.fanoutText(ok, render.TSV, 1000); err != nil {
		t.Fatal(err)
	}
	// Each gets a tenth of 1000 bytes, less the first line (62 bytes) and the
	// names with their colons and blank lines (88): 85, too few for what a
	// summary says of its file, which made the ten sections take about 1,100
	// bytes. A section that spills now only names its file and counts its rows.
	files := regexp.MustCompile(`(?m)^(ids|names):\nFull result: (\S+)\n\(60 rows\)$`).FindAllStringSubmatch(got, -1)
	if len(got) > 1000 || len(files) != 2 || strings.Count(got, "Full result: ") != 2 ||
		!strings.HasSuffix(got, "\n\nmore7:\nname\nx\n(1 row)") {
		t.Fatalf("ten sections sharing 1000 bytes, %d bytes:\n%s", len(got), got)
	}
	for _, f := range files {
		if _, err := os.Stat(f[2]); err != nil {
			t.Error(err)
		}
	}
}

// Ten sections, each a summary of twenty long column names, fit together in
// the inline limit, with the line that introduces them and their names.
func TestFanoutSectionsFitTogether(t *testing.T) {
	h := &Handlers{spill: &spill.Store{Dir: t.TempDir(), InlineMaxBytes: config.InlineMaxBytes}}
	var ok []fanoutResult
	for i := range 10 {
		res := &result.Result{}
		for c := range 20 {
			name := fmt.Sprintf("%s_%d_%d", strings.Repeat("c", 50), i, c)
			res.Columns = append(res.Columns, result.Column{Name: name, TypeOID: pgtype.TextOID, TypeName: "text"})
		}
		for range 50 {
			row := make([]*string, len(res.Columns))
			for c := range row {
				row[c] = ptr("value")
			}
			res.Rows = append(res.Rows, row)
		}
		ok = append(ok, fanoutResult{conn: fmt.Sprintf("connection_%d", i), res: res})
	}
	got, err := h.fanoutText(ok, render.TSV, config.InlineMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "\nFull result: "); len(got) > config.InlineMaxBytes || n != 10 {
		t.Errorf("%d bytes naming %d spill files, want at most %d naming 10:\n%s", len(got), n, config.InlineMaxBytes, got)
	}
}
