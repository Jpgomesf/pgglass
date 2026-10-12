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
