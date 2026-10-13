package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/result"
)

// script is an engine.Querier that answers the n-th query with the n-th reply.
type script struct {
	replies []reply
	queries int
}

type reply struct {
	res *result.Result
	err error
}

func (s *script) Query(context.Context, engine.Query) (*result.Result, error) {
	s.queries++
	if s.queries > len(s.replies) {
		return nil, errors.New("script: unexpected query")
	}
	r := s.replies[s.queries-1]
	return r.res, r.err
}

// row is a reply of one row holding values; a nil value is NULL.
func row(values ...any) reply {
	cells := make([]*string, len(values))
	for i, v := range values {
		if s, ok := v.(string); ok {
			cells[i] = &s
		}
	}
	return reply{res: &result.Result{Columns: make([]result.Column, len(cells)), Rows: [][]*string{cells}}}
}

// The queries Audit runs, in order, when the session user is the current user.
const (
	usersQuery = iota
	attributesQuery
	membershipsQuery
	superusersQuery
	tableWritesQuery
	criticalFunctionsQuery
	warningFunctionsQuery
)

// clean returns the replies Postgres gives for a role with nothing to report.
func clean() []reply {
	return []reply{
		usersQuery:             row("app", "app"),
		attributesQuery:        row("f", "f", "f", "f"),
		membershipsQuery:       row(nil),
		superusersQuery:        row("0", nil),
		tableWritesQuery:       row("0", nil),
		criticalFunctionsQuery: row("0", nil),
		warningFunctionsQuery:  row("0", nil),
	}
}

func critical(check, detail string) preflight.Finding {
	return preflight.Finding{Severity: preflight.SeverityCritical, Check: check, Detail: detail}
}

func warning(check, detail string) preflight.Finding {
	return preflight.Finding{Severity: preflight.SeverityWarning, Check: check, Detail: detail}
}

func wantFindings(t *testing.T, report preflight.Report, want ...preflight.Finding) {
	t.Helper()
	if !slices.Equal(report.Findings, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", report, preflight.Report{Role: report.Role, Findings: want})
	}
}

func TestAuditCleanRole(t *testing.T) {
	s := &script{replies: clean()}
	report, err := preflight.Audit(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if report.Role != "app" || s.queries != len(s.replies) {
		t.Errorf("role = %q after %d queries, want app after %d", report.Role, s.queries, len(s.replies))
	}
	wantFindings(t, report)
}

func TestAuditSuperuserStopsAtItsAttributes(t *testing.T) {
	s := &script{replies: []reply{row("admin", "admin"), row("t", "t", "t", "t")}}
	report, err := preflight.Audit(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if report.Role != "admin" || s.queries != 2 {
		t.Errorf("role = %q after %d queries, want admin after 2", report.Role, s.queries)
	}
	wantFindings(t, report, critical("superuser", "bypasses every privilege check"))
}

func TestAuditReportsEveryFindingCriticalFirst(t *testing.T) {
	replies := clean()
	replies[attributesQuery] = row("f", "t", "t", "t")
	replies[membershipsQuery] = row("pg_execute_server_program pg_read_server_files pg_signal_backend pg_write_server_files")
	replies[tableWritesQuery] = row("12", "INSERT on s.t1; DELETE on s.t2")
	replies[criticalFunctionsQuery] = row("1", "net.http_post")
	replies[warningFunctionsQuery] = row("3", "a.f, b.g, c.h")

	report, err := preflight.Audit(t.Context(), &script{replies: replies})
	if err != nil {
		t.Fatal(err)
	}
	wantFindings(t, report,
		critical("role_membership", "member of pg_execute_server_program"),
		critical("role_membership", "member of pg_write_server_files"),
		critical("function_execute", "EXECUTE on net.http_post"),
		warning("bypassrls", "ignores row-level security"),
		warning("createrole", "can create roles"),
		warning("createdb", "can create databases"),
		warning("role_membership", "member of pg_read_server_files"),
		warning("role_membership", "member of pg_signal_backend"),
		warning("table_write", "12 relations, first 10: INSERT on s.t1; DELETE on s.t2"),
		warning("function_execute", "EXECUTE on 3 functions: a.f, b.g, c.h"),
	)
}

// A query that fails or does not return one complete row fails the whole
// audit: a partial report could pass for a clean role.
func TestAuditFailsClosed(t *testing.T) {
	boom := errors.New("connection reset")
	failures := map[string]struct {
		reply reply
		want  string // in the error message
	}{
		"query error": {reply{err: boom}, boom.Error()},
		"nil result":  {reply{}, "no result"},
		"truncated":   {reply{res: &result.Result{Truncated: true, TruncatedReason: "max_bytes=8"}}, "truncated (max_bytes=8)"},
		"no rows":     {reply{res: &result.Result{}}, "got 0 rows"},
		"two rows":    {reply{res: &result.Result{Rows: [][]*string{{}, {}}}}, "got 2 rows"},
		"short row":   {row(), "got 1 rows"},
	}
	checks := []string{
		usersQuery:             "current and session user",
		attributesQuery:        "role attributes",
		membershipsQuery:       "role memberships",
		superusersQuery:        "superuser memberships",
		tableWritesQuery:       "table writes",
		criticalFunctionsQuery: "function grants",
		warningFunctionsQuery:  "function grants",
	}
	for query, check := range checks {
		for name, failure := range failures {
			t.Run(fmt.Sprintf("query %d/%s", query, name), func(t *testing.T) {
				replies := clean()
				replies[query] = failure.reply
				report, err := preflight.Audit(t.Context(), &script{replies: replies})
				if err == nil || !strings.Contains(err.Error(), "preflight: "+check+": ") || !strings.Contains(err.Error(), failure.want) {
					t.Fatalf("error = %v, want one naming %q and %q", err, check, failure.want)
				}
				if failure.reply.err != nil && !errors.Is(err, failure.reply.err) {
					t.Errorf("error = %v, does not wrap %v", err, failure.reply.err)
				}
				if report.Role != "" || report.Findings != nil {
					t.Errorf("a failed audit returned %+v, want an empty report", report)
				}
			})
		}
	}
}

func TestAuditRejectsMalformedValues(t *testing.T) {
	tests := map[string]struct {
		query int
		reply reply
	}{
		"boolean":      {attributesQuery, row("maybe", "f", "f", "f")},
		"NULL boolean": {attributesQuery, row("f", "f", nil, "f")},
		"count":        {tableWritesQuery, row("many", nil)},
		"NULL count":   {criticalFunctionsQuery, row(nil, nil)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			replies := clean()
			replies[tt.query] = tt.reply
			_, err := preflight.Audit(t.Context(), &script{replies: replies})
			if err == nil || !strings.Contains(err.Error(), "invalid syntax") {
				t.Errorf("error = %v, want a parse error", err)
			}
		})
	}
}
