package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/guard"
	"github.com/jpgomesf/mcp-postgres/internal/preflight"
	"github.com/jpgomesf/mcp-postgres/internal/render"
)

var (
	queryErr = &engine.QueryError{
		SQLState: "42703", Message: `column "nam" does not exist`, Position: 8,
		Hint: `Perhaps you meant to reference the column "customers.name".`,
	}
	cleanReport = preflight.Report{Role: "ro_clean"}
	warnReport  = preflight.Report{Role: "ro_reader", Findings: []preflight.Finding{
		{Severity: preflight.SeverityWarning, Check: "role_membership", Detail: "member of pg_read_server_files"},
	}}
	blockReport = preflight.Report{Role: "postgres", Findings: []preflight.Finding{
		{Severity: preflight.SeverityCritical, Check: "superuser", Detail: "bypasses every privilege check"},
	}}
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err               error
		outcome, sqlState string
	}{
		{nil, "ok", ""},
		{&guard.Violation{Reason: "DeleteStmt is not allowed"}, "guard_reject", ""},
		{&blockedError{conn: "su", report: blockReport}, "preflight_block", ""},
		{&dbError{queryErr}, "db_error", "42703"},
		{&dbError{fmt.Errorf("role check failed: %w", queryErr)}, "db_error", "42703"},
		{&dbError{&connError{conn: "staging", err: queryErr}}, "db_error", "42703"},
		{&dbError{errors.New("dial tcp: connection refused")}, "db_error", ""},
		{errors.New("invalid arguments: unexpected EOF"), "error", ""},
	} {
		outcome, sqlState := classify(tc.err)
		if outcome != tc.outcome || sqlState != tc.sqlState {
			t.Errorf("classify(%v) = %s %q, want %s %q", tc.err, outcome, sqlState, tc.outcome, tc.sqlState)
		}
	}
}

func TestErrorText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&guard.Violation{Reason: "SET is not allowed"}, "query rejected: SET is not allowed"},
		{&dbError{queryErr}, `ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8` + "\n" +
			`HINT: Perhaps you meant to reference the column "customers.name".`},
		{&dbError{&engine.QueryError{SQLState: "22P02", Message: "invalid input syntax for type json", Detail: `Token "x" is invalid.`}},
			"ERROR: invalid input syntax for type json (SQLSTATE 22P02)\nDETAIL: Token \"x\" is invalid."},
		{&dbError{errors.New("query cancelled: context canceled")}, "query cancelled: context canceled"},
		// The connection's name starts the text, a Postgres error's too.
		{&dbError{&connError{conn: "staging", err: fmt.Errorf("read the structure of schema %q: %w", "public", queryErr)}},
			`staging: ERROR: column "nam" does not exist (SQLSTATE 42703) at character 8` + "\n" +
				`HINT: Perhaps you meant to reference the column "customers.name".`},
		{&dbError{&connError{conn: "staging", err: errors.New(`schema "x" does not exist`)}}, `staging: schema "x" does not exist`},
		{&blockedError{conn: "su", report: blockReport}, "connection \"su\" refused (role_check strict): its role can do more than read.\n" +
			"role postgres: 1 finding(s)\n  critical superuser: bypasses every privilege check"},
	} {
		if got := errorText(tc.err); got != tc.want {
			t.Errorf("errorText(%v) =\n%s\nwant\n%s", tc.err, got, tc.want)
		}
	}
}

func TestSQLHash(t *testing.T) {
	// echo -n 'SELECT 1' | shasum -a 256
	if got := sqlHash("SELECT 1"); got != "e004ebd5b553" {
		t.Errorf("sqlHash(SELECT 1) = %q", got)
	}
	if got := sqlHash(""); got != "" {
		t.Errorf("sqlHash of no SQL = %q, want empty", got)
	}
}

func TestSearchLimit(t *testing.T) {
	for _, tc := range []struct{ limit, maxRows, want int }{
		{0, 1000, 50},
		{0, 10, 10},
		{5, 1000, 5},
		{5000, 1000, 1000},
	} {
		if got, err := searchLimit(tc.limit, tc.maxRows); err != nil || got != tc.want {
			t.Errorf("searchLimit(%d, %d) = %d, %v; want %d", tc.limit, tc.maxRows, got, err, tc.want)
		}
	}
	if _, err := searchLimit(-1, 1000); err == nil {
		t.Error("searchLimit(-1) succeeded, want an error")
	}
}

func TestFirstLines(t *testing.T) {
	text := strings.Repeat("0123456789\n", 99) + "0123456789" // 100 lines, 1099 bytes
	// A total lower than the lines of text counts those lines instead.
	for _, total := range []int{0, 100} {
		if got := firstLines(text, total, len(text)); got != text {
			t.Errorf("text that fits, total %d, was cut:\n%s", total, got)
		}
	}
	// 38 lines and the note take 497 bytes; a 39th line would not fit.
	want := strings.Repeat("0123456789\n", 38) + "… 38 of 100 lines shown (output limit 500 bytes): use pg_search to find names"
	if got := firstLines(text, 100, 500); got != want || len(got) > 500 {
		t.Errorf("firstLines(text, 100, 500) =\n%s\nwant\n%s", got, want)
	}
	if got, want := firstLines(strings.Repeat("x", 1000), 1, 100), "… 0 of 1 lines shown (output limit 100 bytes): use pg_search to find names"; got != want {
		t.Errorf("a line longer than the limit: %q, want %q", got, want)
	}
}

// Text that fits but holds only the first lines of a listing says how many
// there are in all; so does text cut to the limit.
func TestFirstLinesOfALongerListing(t *testing.T) {
	text := "a.t1: table\na.t2: table"
	if got, want := firstLines(text, 5000, 1000), text+"\n… 2 of 5000 lines shown (output limit 1000 bytes): use pg_search to find names"; got != want {
		t.Errorf("firstLines = %q, want %q", got, want)
	}
	if got, want := firstLines(text, 5000, 100), "a.t1: table\n… 1 of 5000 lines shown (output limit 100 bytes): use pg_search to find names"; got != want {
		t.Errorf("firstLines = %q, want %q", got, want)
	}
}

func TestFormat(t *testing.T) {
	h := &Handlers{cfg: config.Config{Defaults: config.Defaults{Format: config.FormatMarkdown}}}
	for name, want := range map[string]render.Format{"": render.Markdown, "tsv": render.TSV, "json": render.JSON, "markdown": render.Markdown} {
		if got, err := h.format(name); err != nil || got != want {
			t.Errorf("format(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"csv", "TSV", "xml"} {
		if _, err := h.format(name); err == nil {
			t.Errorf("format(%q) succeeded, want an error", name)
		}
	}
}

// The schema argument replaces default_schema, and either is refused when it
// names a schema whose functions the guard denies.
func TestSearchPath(t *testing.T) {
	h := &Handlers{cfg: config.Config{Connections: map[string]config.Connection{
		"app": {DefaultSchema: "app"},
		"net": {DefaultSchema: "net"},
	}}}
	for _, tc := range []struct{ conn, schema, want string }{
		{"app", "", "app"},
		{"app", "public", "public"},
		{"net", "app", "app"},
	} {
		if got, err := h.searchPath(tc.conn, tc.schema); err != nil || got != tc.want {
			t.Errorf("searchPath(%q, %q) = %q, %v; want %q", tc.conn, tc.schema, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ conn, schema string }{{"app", "net"}, {"app", "AWS_S3"}, {"net", ""}} {
		var v *guard.Violation
		if _, err := h.searchPath(tc.conn, tc.schema); !errors.As(err, &v) {
			t.Errorf("searchPath(%q, %q): err = %v, want a *guard.Violation", tc.conn, tc.schema, err)
		}
	}
	// Postgres would cut a longer name to the first 63 bytes, which may name
	// another schema.
	long := strings.Repeat("s", 64)
	h.cfg.Connections["long"] = config.Connection{DefaultSchema: long}
	for _, tc := range []struct{ conn, schema string }{{"app", long}, {"long", ""}} {
		const want = "schema is 64 bytes: Postgres names take at most 63, and it cuts a longer one"
		if _, err := h.searchPath(tc.conn, tc.schema); err == nil || err.Error() != want {
			t.Errorf("searchPath(%q, %d bytes) = %v, want %q", tc.conn, len(tc.schema), err, want)
		}
	}
}

// Postgres keeps names of at most 63 bytes and cuts a longer one to its
// first 63, which may name another object: checkName refuses it.
func TestCheckName(t *testing.T) {
	for _, name := range []string{"", "orders", strings.Repeat("n", 63), strings.Repeat("é", 31) + "x"} {
		if err := checkName("table", name); err != nil {
			t.Errorf("checkName of %d bytes: %v", len(name), err)
		}
	}
	for _, name := range []string{strings.Repeat("n", 64), strings.Repeat("é", 32), strings.Repeat("n", 12000)} {
		want := fmt.Sprintf("table is %d bytes: Postgres names take at most 63, and it cuts a longer one", len(name))
		if err := checkName("table", name); err == nil || err.Error() != want {
			t.Errorf("checkName of %d bytes = %v, want %q", len(name), err, want)
		}
	}
}

func TestDecode(t *testing.T) {
	var args queryArgs
	if err := decode([]byte(`{"connection": "c", "sql": "SELECT $1", "params": [12345678901234567890.5, "x", null], "max_rows": 5}`), &args); err != nil {
		t.Fatal(err)
	}
	if args.Connection != "c" || args.SQL != "SELECT $1" || len(args.Params) != 3 || args.MaxRows != 5 {
		t.Errorf("decoded %+v", args)
	}
	if n, ok := args.Params[0].(json.Number); !ok || n != "12345678901234567890.5" {
		t.Errorf("number param = %#v, want its exact text", args.Params[0])
	}
	if err := decode(nil, &args); err != nil {
		t.Errorf("no arguments: %v", err)
	}
	for _, raw := range []string{`{"query": "SELECT 1"}`, `{"sql": 1}`, `{"max_rows": 1.5}`, `[`} {
		if err := decode([]byte(raw), &queryArgs{}); err == nil || !strings.HasPrefix(err.Error(), "invalid arguments") {
			t.Errorf("decode(%s) = %v, want an invalid arguments error", raw, err)
		}
	}
}

func TestCheckParams(t *testing.T) {
	if err := checkParams([]any{"x", json.Number("1.5"), true, nil}); err != nil {
		t.Errorf("scalars: %v", err)
	}
	for _, p := range []any{[]any{json.Number("1")}, map[string]any{"a": json.Number("1")}} {
		err := checkParams([]any{"x", p})
		if err == nil || !strings.HasPrefix(err.Error(), "params[2] is an array or object") {
			t.Errorf("checkParams(%v) = %v", p, err)
		}
	}
}

// fakeChecks returns roleChecks whose audit returns the next of results and
// counts its calls.
func fakeChecks(mode string, results ...func() (preflight.Report, error)) (*roleChecks, *atomic.Int32) {
	var calls atomic.Int32
	rc := &roleChecks{mode: mode, conns: map[string]*roleCheck{}}
	rc.audit = func(context.Context, string) (preflight.Report, error) {
		n := int(calls.Add(1)) - 1
		return results[min(n, len(results)-1)]()
	}
	return rc, &calls
}

func report(r preflight.Report) func() (preflight.Report, error) {
	return func() (preflight.Report, error) { return r, nil }
}

func failure(err error) func() (preflight.Report, error) {
	return func() (preflight.Report, error) { return preflight.Report{}, err }
}

func TestGateStrict(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckStrict, report(blockReport))
	for range 2 {
		_, err := rc.gate(t.Context(), "su")
		var blocked *blockedError
		if !errors.As(err, &blocked) {
			t.Errorf("gate = %v, want a *blockedError", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("audited %d times, want once", n)
	}
	if got := rc.status("su"); !strings.Contains(got, "critical superuser") || !strings.HasSuffix(got, "refused: role_check is strict") {
		t.Errorf("status = %q", got)
	}

	// Warnings alone do not block, and show once.
	rc, _ = fakeChecks(config.RoleCheckStrict, report(warnReport))
	if notice, err := rc.gate(t.Context(), "reader"); err != nil || !strings.Contains(notice, "warning role_membership") {
		t.Errorf("first gate = %q, %v; want the warning", notice, err)
	}
	if notice, err := rc.gate(t.Context(), "reader"); err != nil || notice != "" {
		t.Errorf("second gate = %q, %v; want no notice", notice, err)
	}
}

func TestGateWarnShowsFindingsOnce(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckWarn, report(blockReport))
	notice, err := rc.gate(t.Context(), "su")
	if err != nil || notice != "role check: "+blockReport.String()+"\n\n" {
		t.Errorf("first gate = %q, %v", notice, err)
	}
	if notice, err := rc.gate(t.Context(), "su"); err != nil || notice != "" {
		t.Errorf("second gate = %q, %v; want no notice", notice, err)
	}
	if calls.Load() != 1 {
		t.Errorf("audited %d times, want once", calls.Load())
	}
	if got := rc.status("su"); got != blockReport.String() {
		t.Errorf("status = %q, want the report", got)
	}

	rc, _ = fakeChecks(config.RoleCheckWarn, report(cleanReport))
	if notice, err := rc.gate(t.Context(), "clean"); err != nil || notice != "" {
		t.Errorf("clean role: gate = %q, %v; want no notice", notice, err)
	}
	if got := rc.status("clean"); got != "role ro_clean: no findings" {
		t.Errorf("status = %q", got)
	}
}

func TestGateRetriesFailedAudits(t *testing.T) {
	boom := errors.New("connection refused")

	rc, calls := fakeChecks(config.RoleCheckStrict, failure(boom), report(cleanReport))
	if got := rc.status("db"); got != "role not checked yet: checked on first use" {
		t.Errorf("status before any audit = %q", got)
	}
	_, err := rc.gate(t.Context(), "db")
	var dbErr *dbError
	if !errors.As(err, &dbErr) || !errors.Is(err, boom) {
		t.Errorf("strict gate after a failed audit = %v, want a *dbError wrapping it", err)
	}
	if got := rc.status("db"); got != "role check failed: connection refused" {
		t.Errorf("status after a failed audit = %q", got)
	}
	if notice, err := rc.gate(t.Context(), "db"); err != nil || notice != "" {
		t.Errorf("gate after a successful retry = %q, %v", notice, err)
	}
	if calls.Load() != 2 {
		t.Errorf("audited %d times, want 2", calls.Load())
	}

	rc, _ = fakeChecks(config.RoleCheckWarn, failure(boom))
	if notice, err := rc.gate(t.Context(), "db"); err != nil || !strings.Contains(notice, "role check failed") {
		t.Errorf("warn gate after a failed audit = %q, %v; want a notice and no error", notice, err)
	}
}

func TestGateOffSkipsTheAudit(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckOff, report(blockReport))
	if notice, err := rc.gate(t.Context(), "su"); err != nil || notice != "" {
		t.Errorf("gate = %q, %v", notice, err)
	}
	if calls.Load() != 0 || rc.status("su") != "role check off" {
		t.Errorf("audited %d times, status %q", calls.Load(), rc.status("su"))
	}
}

// A call that ends while another call audits its connection stops waiting.
func TestGateStopsWaitingWhenTheCallEnds(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseAudit := sync.OnceFunc(func() { close(release) })
	defer releaseAudit()
	rc, calls := fakeChecks(config.RoleCheckStrict, func() (preflight.Report, error) {
		close(started)
		<-release
		return cleanReport, nil
	})
	first := make(chan error, 1)
	go func() {
		_, err := rc.gate(context.Background(), "db")
		first <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() {
		_, err := rc.gate(ctx, "db")
		second <- err
	}()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("gate after its context ended = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate kept waiting for the audit after its context ended")
	}
	if got := rc.status("db"); got != "role not checked yet: checked on first use" {
		t.Errorf("status = %q: an ended call is not a failed audit", got)
	}

	releaseAudit()
	if err := <-first; err != nil || calls.Load() != 1 {
		t.Errorf("first gate = %v after %d audits, want nil after 1", err, calls.Load())
	}
}

func TestGateAuditsOnceUnderConcurrency(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckWarn, report(warnReport))
	var (
		wg      sync.WaitGroup
		notices atomic.Int32
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if notice, _ := rc.gate(context.Background(), "reader"); notice != "" {
				notices.Add(1)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || notices.Load() != 1 {
		t.Errorf("%d audits and %d notices, want one of each", calls.Load(), notices.Load())
	}
}
